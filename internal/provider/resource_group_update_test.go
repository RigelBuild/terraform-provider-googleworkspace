package googleworkspace

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	directory "google.golang.org/api/admin/directory/v1"
)

const (
	groupOldEmail = "old@example.com"
	groupNewEmail = "new@example.com"
	groupID       = "group-id"
)

func fakeAdminCountRequests(requests []string, prefix string) int {
	count := 0
	for _, request := range requests {
		if strings.HasPrefix(request, prefix) {
			count++
		}
	}
	return count
}

func TestResourceGroupUpdate_KeepAliasRename(t *testing.T) {
	state, result, requests, diags := applyGroupUpdate(t, directory.Group{
		Id: groupID, Email: groupOldEmail,
	}, map[string]interface{}{
		"email": groupNewEmail, "aliases": []interface{}{groupOldEmail},
	}, 0, "5s")
	if diags.HasError() {
		t.Fatalf("Apply failed: %v; requests: %v", diags, requests)
	}
	if state == nil || result == nil {
		t.Fatal("Apply returned no state")
	}
	if got := groupAliases(t, result); len(got) != 1 || got[0] != groupOldEmail {
		t.Fatalf("aliases = %v, want [%s]", got, groupOldEmail)
	}
	groupUpdate := requestIndex(requests, "PUT /admin/directory/v1/groups/")
	aliasWrite := firstAliasWriteIndex(requests)
	if groupUpdate < 0 || (aliasWrite >= 0 && groupUpdate > aliasWrite) {
		t.Fatalf("group update should precede alias writes: %v", requests)
	}
	if fakeAdminCountRequests(requests, "POST /admin/directory/v1/groups/") != 0 {
		t.Fatalf("declared old primary was inserted as an alias: %v", requests)
	}
}

func TestResourceGroupUpdate_DropOldRename(t *testing.T) {
	_, result, requests, diags := applyGroupUpdate(t, directory.Group{
		Id: groupID, Email: groupOldEmail,
	}, map[string]interface{}{
		"email": groupNewEmail, "aliases": []interface{}{},
	}, 0, "5s")
	if diags.HasError() {
		t.Fatalf("Apply failed: %v", diags)
	}
	if got := groupAliases(t, result); len(got) != 0 {
		t.Fatalf("aliases = %v, want none", got)
	}
	groupUpdate := requestIndex(requests, "PUT /admin/directory/v1/groups/")
	oldAliasDelete := requestIndexContaining(requests, "DELETE ", groupOldEmail)
	if groupUpdate < 0 || oldAliasDelete <= groupUpdate {
		t.Fatalf("auto-alias delete should follow the group update: %v", requests)
	}
}

func TestResourceGroupUpdate_PromoteOwnAlias(t *testing.T) {
	_, result, requests, diags := applyGroupUpdate(t, directory.Group{
		Id: groupID, Email: groupOldEmail, Aliases: []string{groupNewEmail},
	}, map[string]interface{}{
		"email": groupNewEmail, "aliases": []interface{}{},
	}, 0, "5s")
	if diags.HasError() {
		t.Fatalf("Apply failed: %v", diags)
	}
	deleteTarget := requestIndexContaining(requests, "DELETE ", groupNewEmail)
	groupUpdate := requestIndex(requests, "PUT /admin/directory/v1/groups/")
	if deleteTarget < 0 || groupUpdate <= deleteTarget {
		t.Fatalf("target alias delete should precede group update: %v", requests)
	}
	if got := groupAliases(t, result); len(got) != 0 {
		t.Fatalf("aliases = %v, want none", got)
	}
}

func TestResourceGroupUpdate_AutoAliasNeverAppears(t *testing.T) {
	_, _, _, diags := applyGroupUpdate(t, directory.Group{
		Id: groupID, Email: groupOldEmail,
	}, map[string]interface{}{
		"email": groupNewEmail, "aliases": []interface{}{},
	}, -1, "100ms")
	if !diags.HasError() {
		t.Fatal("Apply succeeded without the old primary becoming an alias")
	}
	if !strings.Contains(fmt.Sprint(diags), groupOldEmail) {
		t.Fatalf("error does not name missing address %s: %v", groupOldEmail, diags)
	}
}

func TestResourceGroupUpdate_DelayedAutoAlias(t *testing.T) {
	_, result, requests, diags := applyGroupUpdate(t, directory.Group{
		Id: groupID, Email: groupOldEmail,
	}, map[string]interface{}{
		"email": groupNewEmail, "aliases": []interface{}{},
	}, 3, "5s")
	if diags.HasError() {
		t.Fatalf("Apply failed: %v", diags)
	}
	groupUpdate := requestIndex(requests, "PUT /admin/directory/v1/groups/")
	deleteOld := requestIndexContaining(requests, "DELETE ", groupOldEmail)
	readsBetween := 0
	for i := groupUpdate + 1; i < deleteOld && i >= 0; i++ {
		if strings.HasPrefix(requests[i], "GET ") {
			readsBetween++
		}
	}
	if groupUpdate < 0 || deleteOld <= groupUpdate || readsBetween < 3 {
		t.Fatalf("old auto-alias must be observed after three reads before deletion: %v", requests)
	}
	if got := groupAliases(t, result); len(got) != 0 {
		t.Fatalf("aliases = %v, want none", got)
	}
}

func TestResourceGroupUpdate_TargetIsNonEditableAlias(t *testing.T) {
	_, _, requests, diags := applyGroupUpdate(t, directory.Group{
		Id: groupID, Email: groupOldEmail, NonEditableAliases: []string{groupNewEmail},
	}, map[string]interface{}{
		"email": groupNewEmail, "aliases": []interface{}{},
	}, 0, "5s")
	if !diags.HasError() {
		t.Fatal("Apply succeeded when target email is a non-editable alias")
	}
	if !strings.Contains(fmt.Sprint(diags), groupNewEmail) {
		t.Fatalf("error does not name non-editable address %s: %v", groupNewEmail, diags)
	}
	if requestIndex(requests, "PATCH ") >= 0 || firstAliasWriteIndex(requests) >= 0 {
		t.Fatalf("non-editable target must fail before any writes: %v", requests)
	}
}

func applyGroupUpdate(
	t *testing.T,
	seed directory.Group,
	config map[string]interface{},
	autoAliasReads int,
	updateTimeout string,
) (*terraform.InstanceState, *terraform.InstanceState, []string, diag.Diagnostics) {
	previousInterval := retryPollIntervalOverride
	retryPollIntervalOverride = time.Millisecond
	t.Cleanup(func() { retryPollIntervalOverride = previousInterval })
	fake := newFakeAdmin(t)
	fake.autoAliasAfterReads = autoAliasReads
	fake.seedGroup(&seed)
	client := newFakeAdminClient(t, fake)

	resource := resourceGroup()
	stateData := schema.TestResourceDataRaw(t, resource.Schema, map[string]interface{}{
		"email":   seed.Email,
		"aliases": stringSliceToInterfaces(seed.Aliases),
	})
	stateData.SetId(seed.Id)
	state := stateData.State()

	config["timeouts"] = map[string]interface{}{"update": updateTimeout}
	diff, err := resource.Diff(context.Background(), state, terraform.NewResourceConfigRaw(config), client)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff == nil {
		t.Fatal("Diff returned nil")
	}
	result, diags := resource.Apply(context.Background(), state, diff, client)
	return state, result, fake.requestLog(), diags
}

func groupAliases(t *testing.T, state *terraform.InstanceState) []string {
	t.Helper()
	if state == nil {
		t.Fatal("state is nil")
	}
	values, ok := resourceGroup().Data(state).Get("aliases").([]interface{})
	if !ok {
		t.Fatalf("aliases state has unexpected type: %T", resourceGroup().Data(state).Get("aliases"))
	}
	aliases := make([]string, 0, len(values))
	for _, value := range values {
		aliases = append(aliases, value.(string))
	}
	return aliases
}

func stringSliceToInterfaces(values []string) []interface{} {
	result := make([]interface{}, len(values))
	for i, value := range values {
		result[i] = value
	}
	return result
}

func requestIndex(requests []string, prefix string) int {
	for i, request := range requests {
		if strings.HasPrefix(request, prefix) {
			return i
		}
	}
	return -1
}

func requestIndexContaining(requests []string, prefix, value string) int {
	for i, request := range requests {
		if strings.HasPrefix(request, prefix) &&
			(strings.Contains(request, value) || strings.Contains(request, urlPathEscape(value))) {
			return i
		}
	}
	return -1
}

func firstAliasWriteIndex(requests []string) int {
	for i, request := range requests {
		if (strings.HasPrefix(request, "DELETE ") || strings.HasPrefix(request, "POST ")) &&
			strings.Contains(request, "/aliases/") ||
			strings.HasPrefix(request, "POST ") && strings.Contains(request, "/aliases?") {
			return i
		}
	}
	return -1
}

func urlPathEscape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}
