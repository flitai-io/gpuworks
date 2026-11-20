package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (a *apiHandlers) createNetworkAttachment(w http.ResponseWriter, r *http.Request) {
	var req dto.NetworkAttachmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	networkID := parseUint(chi.URLParam(r, "id"))
	_, err := a.networks().Attach(networkID, network.AttachmentSpec{
		NodeUUID: req.NodeUUID, LabelKey: req.LabelKey, LabelValue: req.LabelValue,
		Overrides: network.Overrides{Interface: req.Interface, IP: req.IP, HCAs: req.HCAs},
	})
	if err != nil {
		writeNetworkErr(w, err)
		return
	}
	a.writeNetworkDetail(w, http.StatusCreated, networkID)
}

func (a *apiHandlers) updateNetworkAttachment(w http.ResponseWriter, r *http.Request) {
	var req dto.NetworkOverrideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	networkID := parseUint(chi.URLParam(r, "id"))
	overrides := network.Overrides{Interface: req.Interface, IP: req.IP, HCAs: req.HCAs}
	if _, err := a.networks().UpdateOverrides(networkID, parseUint(chi.URLParam(r, "attachmentID")), overrides); err != nil {
		writeNetworkErr(w, err)
		return
	}
	a.writeNetworkDetail(w, http.StatusOK, networkID)
}

func (a *apiHandlers) updateNetworkLabel(w http.ResponseWriter, r *http.Request) {
	var req dto.NetworkLabelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	networkID := parseUint(chi.URLParam(r, "id"))
	if err := a.networks().UpdateLabel(networkID, parseUint(chi.URLParam(r, "attachmentID")), req.LabelKey, req.LabelValue); err != nil {
		writeNetworkErr(w, err)
		return
	}
	a.writeNetworkDetail(w, http.StatusOK, networkID)
}

func (a *apiHandlers) deleteNetworkAttachment(w http.ResponseWriter, r *http.Request) {
	networkID := parseUint(chi.URLParam(r, "id"))
	if err := a.networks().Detach(networkID, parseUint(chi.URLParam(r, "attachmentID"))); err != nil {
		writeNetworkErr(w, err)
		return
	}
	a.writeNetworkDetail(w, http.StatusOK, networkID)
}

// getNodeNetworks reports the networks a node loaded and its inventory.
func (a *apiHandlers) getNodeNetworks(w http.ResponseWriter, r *http.Request) {
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	resolved, err := a.networks().NodeNetworks(node.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := dto.NodeNetworks{Networks: []dto.NodeLoadedNetwork{}, Management: nodeManagementDTO(*node), Interfaces: []dto.NetInterface{}, RDMAPorts: []dto.RDMAPort{}, AcceleratorPorts: []dto.AcceleratorPort{}}
	inv := network.Inventory(*node)
	for _, res := range resolved.Of(node.ID) {
		hcaSource, bootstrapSource := endpointSources(res)
		out.Networks = append(out.Networks, dto.NodeLoadedNetwork{
			Role: res.Role, NetworkID: res.Network.ID, NetworkName: res.Network.Name, NetworkType: res.Network.Type,
			AttachmentID: res.AttachmentID, Source: res.Source, LabelKey: res.LabelKey, LabelValue: res.LabelValue,
			Interface: res.Interface, IP: res.IP, HCAs: railDTOs(inv, res.Network.Type, res.HCAs), LinkLayer: res.LinkLayer,
			HCASource: hcaSource, BootstrapSource: bootstrapSource, Warnings: warningDTOs(res.Warnings),
		})
	}
	if ip := netplan.ServingIP(node.ID, resolved); ip != "" {
		forward := &dto.ServingForward{Address: net.JoinHostPort(ip, strconv.Itoa(proto.ServingForwardPort))}
		if st, ok := store.NodeServingForward(node.ServingForward); ok {
			at := st.At
			forward.Mode, forward.Reason, forward.At = st.Mode, st.Reason, &at
		}
		out.ServingForward = forward
	}
	if inv != nil {
		out.InventoryReported = true
		rates := a.latestLinkRates(node.UUID)
		for _, i := range inv.Interfaces {
			rx, tx := rates.nic(i.Name)
			out.Interfaces = append(out.Interfaces, dto.NetInterface{Name: i.Name, State: i.State, MTU: i.MTU, SpeedMbps: i.SpeedMbps, MAC: i.MAC, Addrs: nonNilStrings(i.Addrs), RxBPS: rx, TxBPS: tx})
		}
		rails := netplan.AllRails(inv, netplan.TypeHostRDMA)
		for i, p := range inv.RDMAPorts {
			rx, tx := rates.rdma(fmt.Sprintf("%s:%d", p.Device, p.Port))
			out.RDMAPorts = append(out.RDMAPorts, dto.RDMAPort{Device: p.Device, Port: p.Port, State: p.State, LinkLayer: p.LinkLayer, RateGbps: p.RateGbps, NetDev: p.NetDev,
				IP: rails[i].IP(), Gap: rails[i].Gap(), RxBPS: rx, TxBPS: tx})
		}
		cards := netplan.AllRails(inv, netplan.TypeAcceleratorRDMA)
		hccsCards := netplan.AllRails(inv, netplan.TypeAcceleratorHCCS)
		for i, p := range inv.AcceleratorPorts {
			port := dto.AcceleratorPort{Name: p.Name, Index: p.Index, IP: p.IP, Netmask: p.Netmask, State: p.State, SpeedGbps: p.SpeedGbps, HCCSIP: p.HCCSIP, HCCSNetmask: p.HCCSNetmask, HCCSState: p.HCCSState, Gap: cards[i].Gap()}
			if p.HCCSIP != "" || p.HCCSNetmask != "" || p.HCCSState != "" {
				port.HCCSGap = hccsCards[i].Gap()
			}
			out.AcceleratorPorts = append(out.AcceleratorPorts, port)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
