package network

import (
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// What an InvalidError rejects.
const (
	InvalidName     = "name"      // missing or longer than 64 characters
	InvalidRole     = "role"      // Value is the unknown role, "" when none was given
	InvalidType     = "type"      // Value is the unknown type
	InvalidTypeRole = "type_role" // Value is a role the type cannot carry, Field the type
	InvalidSelector = "selector"  // Field is cidr or interface
	InvalidOverride = "override"  // Field is interface, ip, hcas or target
	InvalidTarget   = "target"    // an attachment names neither or both of a node and a label; Value is the node
)

// InvalidError rejects a network, attachment or override the caller sent.
type InvalidError struct {
	Kind   string // Invalid*
	Field  string
	Value  string
	Detail string // why, when more than Kind says
}

func (e *InvalidError) Error() string {
	msg := "invalid network " + e.Kind
	if e.Field != "" {
		msg += " " + e.Field
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// ErrAttachmentNotFound means the network has no such attachment.
var ErrAttachmentNotFound = errors.New("network attachment not found")

// ErrManagementRolesConflict rejects a batch that both allows and denies the
// same role.
var ErrManagementRolesConflict = errors.New("a role is both allowed and denied")

var (
	interfaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,32}$`)
	hcaNamePattern       = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}(:[0-9]{1,3})?$`)
)

// NetworkSpec is a network as the operator declares it.
//
//nolint:revive // Preserve the exported network API type used by planning callers.
type NetworkSpec struct {
	Name, Description string
	Roles             []string
	Type              string
	// Selector. An RDMA network takes only a bootstrap CIDR: its rails and
	// bootstrap are inferred per node.
	CIDR, Interface string
}

// Overrides pin a node attachment to an interface, IP or RDMA devices.
type Overrides struct {
	Interface string
	IP        string
	HCAs      []string
}

// AttachmentSpec attaches a network to exactly one of a node or a label.
// Overrides apply to node attachments only.
type AttachmentSpec struct {
	NodeUUID   string
	LabelKey   string
	LabelValue string
	Overrides  Overrides
}

// CreateNetwork validates and stores a new network.
func (s *Service) CreateNetwork(spec NetworkSpec) (*store.Network, error) {
	n, err := s.validNetwork(spec)
	if err != nil {
		return nil, err
	}
	return s.store.CreateNetwork(n)
}

// UpdateNetwork validates and replaces a network's fields.
func (s *Service) UpdateNetwork(id uint, spec NetworkSpec) (*store.Network, error) {
	n, err := s.validNetwork(spec)
	if err != nil {
		return nil, err
	}
	n.ID = id
	return s.store.UpdateNetwork(n)
}

// DeleteNetwork removes a network with its attachments and test runs.
func (s *Service) DeleteNetwork(id uint) error {
	if !s.Dedicated() {
		return ErrDedicatedUnavailable
	}
	return s.store.DeleteNetwork(id)
}

func (s *Service) validNetwork(spec NetworkSpec) (*store.Network, error) {
	if !s.Dedicated() {
		return nil, ErrDedicatedUnavailable
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" || len(name) > 64 {
		return nil, &InvalidError{Kind: InvalidName}
	}
	roles, err := netplan.CanonicalRoles(spec.Roles)
	var unknown *netplan.UnknownRoleError
	if errors.As(err, &unknown) {
		return nil, &InvalidError{Kind: InvalidRole, Value: unknown.Role}
	}
	if err != nil || len(roles) == 0 {
		return nil, &InvalidError{Kind: InvalidRole}
	}
	typ := strings.TrimSpace(spec.Type)
	if !netplan.ValidType(typ) {
		return nil, &InvalidError{Kind: InvalidType, Value: typ}
	}
	for _, role := range roles {
		if !netplan.RoleAllowed(typ, role) {
			return nil, &InvalidError{Kind: InvalidTypeRole, Field: typ, Value: role}
		}
	}
	n := &store.Network{
		Name: name, Description: strings.TrimSpace(spec.Description), Roles: store.EncodeStringList(roles), Type: typ,
		CIDR: strings.TrimSpace(spec.CIDR), Interface: strings.TrimSpace(spec.Interface),
	}
	if n.CIDR != "" {
		_, subnet, err := net.ParseCIDR(n.CIDR)
		if err != nil {
			return nil, &InvalidError{Kind: InvalidSelector, Field: "cidr", Value: n.CIDR}
		}
		n.CIDR = subnet.String()
	}
	if !validGlob(n.Interface) || (netplan.UsesRails(typ) && n.Interface != "") {
		return nil, &InvalidError{Kind: InvalidSelector, Field: "interface", Value: n.Interface}
	}
	return n, nil
}

// validGlob accepts an empty selector or a path.Match pattern without spaces.
func validGlob(pattern string) bool {
	if pattern == "" {
		return true
	}
	if strings.ContainsAny(pattern, " \t\r\n/") {
		return false
	}
	_, err := path.Match(pattern, "")
	return err == nil
}

// Attach attaches a network to a node or a label.
func (s *Service) Attach(networkID uint, spec AttachmentSpec) (*store.NetworkAttachment, error) {
	if !s.Dedicated() {
		return nil, ErrDedicatedUnavailable
	}
	network, err := s.store.GetNetwork(networkID)
	if err != nil {
		return nil, err
	}
	att := &store.NetworkAttachment{NetworkID: network.ID}
	nodeUUID, labelKey := strings.TrimSpace(spec.NodeUUID), strings.TrimSpace(spec.LabelKey)
	switch {
	case nodeUUID != "" && labelKey == "":
		node, err := s.store.NodeByUUID(nodeUUID)
		if err != nil {
			return nil, &InvalidError{Kind: InvalidTarget, Value: nodeUUID, Detail: "attachment node not found"}
		}
		att.NodeID = node.ID
		if err := applyOverrides(network.Type, att, spec.Overrides); err != nil {
			return nil, err
		}
	case labelKey != "" && nodeUUID == "":
		o := spec.Overrides
		if strings.TrimSpace(o.Interface) != "" || strings.TrimSpace(o.IP) != "" || len(o.HCAs) > 0 {
			return nil, &InvalidError{Kind: InvalidOverride, Field: "target", Value: "label"}
		}
		value := strings.TrimSpace(spec.LabelValue)
		if err := nodelabel.ValidateWritten(map[string]string{labelKey: value}); err != nil {
			return nil, &InvalidError{Kind: InvalidTarget, Detail: err.Error()}
		}
		att.LabelKey, att.LabelValue = labelKey, value
	default:
		return nil, &InvalidError{Kind: InvalidTarget, Detail: "attach to exactly one of node_uuid or label_key"}
	}
	return s.store.CreateNetworkAttachment(att)
}

// UpdateOverrides replaces a node attachment's overrides.
func (s *Service) UpdateOverrides(networkID, attachmentID uint, o Overrides) (*store.NetworkAttachment, error) {
	if !s.Dedicated() {
		return nil, ErrDedicatedUnavailable
	}
	network, err := s.store.GetNetwork(networkID)
	if err != nil {
		return nil, err
	}
	attachments, err := s.store.ListNetworkAttachments()
	if err != nil {
		return nil, err
	}
	var current *store.NetworkAttachment
	for i := range attachments {
		if attachments[i].ID == attachmentID && attachments[i].NetworkID == network.ID {
			current = &attachments[i]
		}
	}
	if current == nil {
		return nil, ErrAttachmentNotFound
	}
	if current.NodeID == 0 {
		return nil, &InvalidError{Kind: InvalidOverride, Field: "target", Value: "label"}
	}
	if err := applyOverrides(network.Type, current, o); err != nil {
		return nil, err
	}
	return s.store.UpdateNetworkAttachmentOverrides(network.ID, attachmentID, current.Interface, current.IP, store.DecodeStringList(current.HCAs))
}

// Detach removes one attachment of a network.
func (s *Service) Detach(networkID, attachmentID uint) error {
	if !s.Dedicated() {
		return ErrDedicatedUnavailable
	}
	if err := s.store.DeleteNetworkAttachment(networkID, attachmentID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrAttachmentNotFound
		}
		return err
	}
	return nil
}

// applyOverrides validates node-attachment overrides into att. HCAs are only
// meaningful on networks that use rails.
func applyOverrides(networkType string, att *store.NetworkAttachment, o Overrides) error {
	iface, ip := strings.TrimSpace(o.Interface), strings.TrimSpace(o.IP)
	if iface != "" && !interfaceNamePattern.MatchString(iface) {
		return &InvalidError{Kind: InvalidOverride, Field: "interface", Value: iface}
	}
	if ip != "" {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			return &InvalidError{Kind: InvalidOverride, Field: "ip", Value: ip}
		}
		ip = parsed.String()
	}
	var cleaned []string
	seen := map[string]bool{}
	for _, hca := range o.HCAs {
		hca = strings.TrimSpace(hca)
		if hca == "" {
			continue
		}
		if !netplan.UsesRails(networkType) || !hcaNamePattern.MatchString(hca) {
			return &InvalidError{Kind: InvalidOverride, Field: "hcas", Value: hca}
		}
		if !seen[hca] {
			seen[hca] = true
			cleaned = append(cleaned, hca)
		}
	}
	att.Interface, att.IP, att.HCAs = iface, ip, store.EncodeStringList(cleaned)
	return nil
}

// SetManagementRolesOff replaces the roles one node's management network may
// not carry. netplan.ErrUnknownRole rejects an unknown role; store.ErrNotFound
// an unknown node.
func (s *Service) SetManagementRolesOff(uuid string, off []string) error {
	clean, err := netplan.CanonicalRoles(off)
	if err != nil {
		return err
	}
	return s.store.SetNodeManagementRolesOff(uuid, clean)
}

// UpdateManagementRoles lets the management network of every listed node carry
// the allowed roles and stops it carrying the denied ones, leaving each node's
// other roles as they are; all nodes change or none does. It returns how many
// nodes changed. A missing node is a *store.ManagementRolesNotFoundError.
func (s *Service) UpdateManagementRoles(uuids, allow, deny []string) (int, error) {
	allowed, err := netplan.CanonicalRoles(allow)
	if err != nil {
		return 0, err
	}
	denied, err := netplan.CanonicalRoles(deny)
	if err != nil {
		return 0, err
	}
	for _, role := range allowed {
		for _, other := range denied {
			if role == other {
				return 0, fmt.Errorf("%w: %q", ErrManagementRolesConflict, role)
			}
		}
	}
	return s.store.UpdateManagementRolesOff(uuids, func(current []string) []string {
		off := map[string]bool{}
		for _, role := range current {
			off[role] = true
		}
		for _, role := range allowed {
			delete(off, role)
		}
		for _, role := range denied {
			off[role] = true
		}
		next := []string{}
		for _, role := range netplan.Roles {
			if off[role] {
				next = append(next, role)
			}
		}
		return next
	})
}
