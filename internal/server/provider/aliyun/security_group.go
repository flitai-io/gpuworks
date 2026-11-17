package aliyun

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// The server never changes an instance's security groups; for an agent it
// dials, the precheck reads them and says whether the agent's port is open.
// Rules are those of DescribeSecurityGroupAttribute:
// https://help.aliyun.com/zh/ecs/developer-reference/api-ecs-2014-05-26-describesecuritygroupattribute
// Alibaba Cloud evaluates the rules of all an instance's groups together by
// priority (1 first); at equal priority a drop wins; nothing matching drops.

// sgRule is one inbound rule of a security group.
type sgRule struct {
	group    string
	protocol string
	from, to int
	source   string // CIDR, peer group or prefix list
	anywhere bool   // 0.0.0.0/0
	accept   bool
	priority int
}

type apiPermission struct {
	IPProtocol         string          `json:"IpProtocol"`
	PortRange          string          `json:"PortRange"`
	SourceCidrIP       string          `json:"SourceCidrIp"`
	SourceGroupID      string          `json:"SourceGroupId"`
	SourcePrefixListID string          `json:"SourcePrefixListId"`
	Policy             string          `json:"Policy"`
	Priority           json.RawMessage `json:"Priority"`
	Direction          string          `json:"Direction"`
}

// CheckInbound implements provider.InboundRuleChecker.
func (d *Driver) CheckInbound(ctx context.Context, creds provider.Credentials, inst provider.Instance, port int) provision.CheckResult {
	unreadable := func(what string, err error) provision.CheckResult {
		return provision.WarnCheck(provision.CheckIDSecurityGroup, messages.CheckSecurityGroupUnreadable, "could not read "+what, map[string]any{"port": port}).WithEvidence(err.Error())
	}
	found, err := d.findInstance(ctx, creds, inst.ExternalID)
	if err != nil {
		return unreadable("the instance's security groups", err)
	}
	groups := found.SecurityGroupIDs.SecurityGroupID
	if len(groups) == 0 {
		return provision.WarnCheck(provision.CheckIDSecurityGroup, messages.CheckSecurityGroupNone, "the instance lists no security group", map[string]any{"port": port})
	}
	var rules []sgRule
	for _, group := range groups {
		groupRules, err := d.inboundRules(ctx, creds, found.RegionID, group)
		if err != nil {
			return unreadable("security group "+group, err)
		}
		rules = append(rules, groupRules...)
	}
	return inboundVerdict(rules, groups, port)
}

func (d *Driver) inboundRules(ctx context.Context, creds provider.Credentials, region, group string) ([]sgRule, error) {
	var rules []sgRule
	params := map[string]string{"SecurityGroupId": group, "Direction": "ingress", "MaxResults": "1000"}
	for page := 0; page < 20; page++ {
		var out struct {
			Permissions struct {
				Permission []apiPermission `json:"Permission"`
			} `json:"Permissions"`
			NextToken string `json:"NextToken"`
		}
		if err := d.api.call(ctx, creds, ecsProduct, region, "DescribeSecurityGroupAttribute", params, &out); err != nil {
			return nil, err
		}
		for _, perm := range out.Permissions.Permission {
			if rule, ok := perm.rule(group); ok {
				rules = append(rules, rule)
			}
		}
		if out.NextToken == "" {
			return rules, nil
		}
		params["NextToken"] = out.NextToken
	}
	return rules, nil
}

// rule maps an inbound IPv4-relevant permission; IPv6-only rules do not
// apply to the server's IPv4 connection.
func (p apiPermission) rule(group string) (sgRule, bool) {
	if p.Direction != "" && !strings.EqualFold(p.Direction, "ingress") {
		return sgRule{}, false
	}
	r := sgRule{group: group, protocol: strings.ToLower(p.IPProtocol), accept: !strings.EqualFold(p.Policy, "drop"), priority: 1}
	if n, err := strconv.Atoi(strings.Trim(string(p.Priority), `" `)); err == nil {
		r.priority = n
	}
	from, to, ok := strings.Cut(p.PortRange, "/")
	if !ok {
		return sgRule{}, false
	}
	var err1, err2 error
	r.from, err1 = strconv.Atoi(from)
	r.to, err2 = strconv.Atoi(to)
	if err1 != nil || err2 != nil {
		return sgRule{}, false
	}
	switch {
	case p.SourceCidrIP != "":
		r.source, r.anywhere = p.SourceCidrIP, p.SourceCidrIP == "0.0.0.0/0"
	case p.SourceGroupID != "":
		r.source = "security group " + p.SourceGroupID
	case p.SourcePrefixListID != "":
		r.source = "prefix list " + p.SourcePrefixListID
	default:
		return sgRule{}, false
	}
	return r, true
}

func (r sgRule) covers(port int) bool {
	if r.protocol != "tcp" && r.protocol != "all" {
		return false
	}
	return (r.from == -1 && r.to == -1) || (r.from <= port && port <= r.to)
}

// inboundVerdict decides from the rules covering port: the first rule from
// anywhere (by priority, drops first at a tie) settles it, unless a rule for
// specific sources came before it, which may treat the server differently.
func inboundVerdict(rules []sgRule, groups []string, port int) provision.CheckResult {
	var matching []sgRule
	for _, r := range rules {
		if r.covers(port) {
			matching = append(matching, r)
		}
	}
	sort.SliceStable(matching, func(i, j int) bool {
		if matching[i].priority != matching[j].priority {
			return matching[i].priority < matching[j].priority
		}
		return !matching[i].accept && matching[j].accept
	})
	open := fmt.Sprintf("add an inbound rule accepting TCP %d from the server's address to security group %s", port, groups[0])
	var specific []string
	for _, r := range matching {
		if !r.anywhere {
			specific = append(specific, fmt.Sprintf("%s %s (priority %d)", policyWord(r.accept), r.source, r.priority))
			continue
		}
		where := fmt.Sprintf("security group %s (priority %d)", r.group, r.priority)
		params := map[string]any{"group": r.group, "priority": r.priority, "port": port}
		// The rules for specific sources are the provider's own rule text.
		others := strings.Join(specific, ", ")
		switch {
		case r.accept && len(specific) == 0:
			return provision.PassCheck(provision.CheckIDSecurityGroup, messages.CheckSecurityGroupOpen, fmt.Sprintf("%s accepts TCP %d from anywhere", where, port), params)
		case r.accept:
			return provision.WarnCheck(provision.CheckIDSecurityGroup, messages.CheckSecurityGroupOpenAfterSpecific,
				fmt.Sprintf("%s accepts TCP %d from anywhere, after rules for specific sources that may decide first: %s", where, port, others), params).WithEvidence(others)
		case len(specific) == 0:
			return provision.FailCheck(provision.CheckIDSecurityGroup, messages.CheckSecurityGroupDrops,
				fmt.Sprintf("%s drops TCP %d from anywhere, so the server cannot connect; %s with a higher priority", where, port, open), params)
		default:
			return provision.WarnCheck(provision.CheckIDSecurityGroup, messages.CheckSecurityGroupDropsExceptSpecific,
				fmt.Sprintf("%s drops TCP %d from anywhere; only these rules may let the server in: %s", where, port, others), params).WithEvidence(others)
		}
	}
	if len(specific) > 0 {
		others := strings.Join(specific, ", ")
		return provision.WarnCheck(provision.CheckIDSecurityGroup, messages.CheckSecurityGroupSpecificOnly,
			fmt.Sprintf("TCP %d is accepted only from specific sources; the server must connect from one of them: %s", port, others), map[string]any{"port": port}).WithEvidence(others)
	}
	return provision.FailCheck(provision.CheckIDSecurityGroup, messages.CheckSecurityGroupNoRule,
		fmt.Sprintf("no inbound rule of security group %s accepts TCP %d, so the server cannot connect; %s", strings.Join(groups, ", "), port, open),
		map[string]any{"groups": strings.Join(groups, ", "), "port": port})
}

func policyWord(accept bool) string {
	if accept {
		return "accepts from"
	}
	return "drops from"
}
