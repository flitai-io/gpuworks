package aliyun

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// permission is shaped like a DescribeSecurityGroupAttribute Permission item
// (Priority is a string there; a number is read too).
func permission(protocol, ports, source, policy string, priority any) map[string]any {
	return map[string]any{
		"IpProtocol": protocol, "PortRange": ports, "SourceCidrIp": source, "Ipv6SourceCidrIp": "",
		"SourceGroupId": "", "SourcePrefixListId": "", "Policy": policy, "Priority": priority,
		"Direction": "ingress", "NicType": "intranet", "Description": "",
	}
}

func sgFake(t *testing.T, groups map[string][][]map[string]any) (*Driver, *fakeAliyun) {
	ids := []string{}
	for id := range groups {
		ids = append(ids, id)
	}
	return newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeRegions":
			return 200, regionsBody("cn-shanghai")
		case "DescribeInstances":
			inst := instanceJSON("i-sg", "cn-shanghai", "Running", "47.0.0.9", "172.16.0.9", 1, "NVIDIA A10")
			inst["SecurityGroupIds"] = map[string]any{"SecurityGroupId": ids}
			return 200, map[string]any{"Instances": map[string]any{"Instance": []any{inst}}}
		case "DescribeSecurityGroupAttribute":
			if c.query["Direction"] != "ingress" || c.query["RegionId"] != "cn-shanghai" {
				t.Errorf("DescribeSecurityGroupAttribute query = %v", c.query)
			}
			pages := groups[c.query["SecurityGroupId"]]
			page := 0
			if c.query["NextToken"] == "p2" {
				page = 1
			}
			next := ""
			if page+1 < len(pages) {
				next = "p2"
			}
			perms := []any{}
			for _, p := range pages[page] {
				perms = append(perms, p)
			}
			return 200, map[string]any{"SecurityGroupId": c.query["SecurityGroupId"], "Permissions": map[string]any{"Permission": perms}, "NextToken": next, "InnerAccessPolicy": "Accept"}
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound", "Message": c.action}
	})
}

func TestSecurityGroupCheckReadsEveryPageOfTheGroups(t *testing.T) {
	d, fake := sgFake(t, map[string][][]map[string]any{
		"sg-a": {
			{permission("TCP", "22/22", "0.0.0.0/0", "Accept", "1")},
			{permission("TCP", "18000/19000", "0.0.0.0/0", "Accept", 100)},
		},
	})
	check := d.CheckInbound(context.Background(), userCreds, provider.Instance{ExternalID: "i-sg"}, 18800)
	if check.ID != "security_group" || check.Status != "pass" || !strings.Contains(checkText(check), "sg-a (priority 100) accepts TCP 18800 from anywhere") {
		t.Fatalf("check = %+v", check)
	}
	for _, action := range fake.actions() {
		if strings.HasPrefix(action, "Authorize") || strings.HasPrefix(action, "Modify") || strings.HasPrefix(action, "Revoke") {
			t.Fatalf("the check changed a security group: %v", fake.actions())
		}
	}
}

func TestSecurityGroupCheckSaysWhyThePortIsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		perms  []map[string]any
		status string
		detail string
	}{
		"only SSH open": {perms: []map[string]any{permission("TCP", "22/22", "0.0.0.0/0", "Accept", "1")},
			status: "fail", detail: "no inbound rule of security group sg-a accepts TCP 18800"},
		"UDP is not TCP": {perms: []map[string]any{permission("UDP", "18800/18800", "0.0.0.0/0", "Accept", "1")},
			status: "fail", detail: "no inbound rule"},
		"all protocols and ports": {perms: []map[string]any{permission("ALL", "-1/-1", "0.0.0.0/0", "Accept", "1")},
			status: "pass", detail: "accepts TCP 18800 from anywhere"},
		"a drop decides first": {perms: []map[string]any{
			permission("TCP", "18800/18800", "0.0.0.0/0", "Accept", "10"),
			permission("TCP", "1/65535", "0.0.0.0/0", "Drop", "5"),
		}, status: "fail", detail: "(priority 5) drops TCP 18800 from anywhere"},
		"a drop wins a tie": {perms: []map[string]any{
			permission("TCP", "18800/18800", "0.0.0.0/0", "Accept", "1"),
			permission("TCP", "18800/18800", "0.0.0.0/0", "Drop", "1"),
		}, status: "fail", detail: "drops TCP 18800"},
		"only some sources": {perms: []map[string]any{permission("TCP", "18800/18800", "203.0.113.0/24", "Accept", "1")},
			status: "warn", detail: "accepted only from specific sources; the server must connect from one of them: accepts from 203.0.113.0/24"},
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := sgFake(t, map[string][][]map[string]any{"sg-a": {tc.perms}})
			check := d.CheckInbound(context.Background(), userCreds, provider.Instance{ExternalID: "i-sg"}, 18800)
			if check.Status != tc.status || !strings.Contains(checkText(check), tc.detail) {
				t.Fatalf("check = %+v", check)
			}
		})
	}
}

func TestSecurityGroupCheckWarnsWhenTheGroupsCannotBeRead(t *testing.T) {
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeRegions":
			return 200, regionsBody("cn-shanghai")
		case "DescribeInstances":
			inst := instanceJSON("i-sg", "cn-shanghai", "Running", "47.0.0.9", "172.16.0.9", 1, "NVIDIA A10")
			inst["SecurityGroupIds"] = map[string]any{"SecurityGroupId": []string{"sg-a"}}
			return 200, map[string]any{"Instances": map[string]any{"Instance": []any{inst}}}
		}
		return 403, map[string]string{"Code": "Forbidden.RAM", "Message": "User not authorized to operate on the specified resource."}
	})
	check := d.CheckInbound(context.Background(), userCreds, provider.Instance{ExternalID: "i-sg"}, 18800)
	if check.Status != "warn" || !strings.Contains(checkText(check), "could not read security group sg-a") || !strings.Contains(checkText(check), "Forbidden.RAM") {
		t.Fatalf("check = %+v", check)
	}
}
