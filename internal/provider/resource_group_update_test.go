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
	}, 3, "20s")
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
	if requestIndex(requests, "PUT /admin/directory/v1/groups/") >= 0 ||
		requestIndex(requests, "PATCH /admin/directory/v1/groups/") >= 0 ||
		firstAliasWriteIndex(requests) >= 0 {
		t.Fatalf("non-editable target must fail before any writes: %v", requests)
	}
}

func TestResourceGroupUpdate_EmailOnlyRenameKeepsDeclaredAliases(t *testing.T) {
	declaredAlias := "x@example.com"
	fake := newFakeAdmin(t)
	fake.seedGroup(&directory.Group{Id: groupID, Email: groupOldEmail})
	stateAliases := []string{declaredAlias}
	_, result, requests, diags := applyGroupUpdateWithFake(t, fake, groupUpdateInput{
		oldEmail:     groupOldEmail,
		stateAliases: stateAliases,
		config: map[string]interface{}{
			"email": groupNewEmail, "aliases": []interface{}{declaredAlias},
		},
		timeout: "5s",
	})
	if diags.HasError() {
		t.Fatalf("Apply failed: %v; requests: %v", diags, requests)
	}
	got := groupAliases(t, result)
	if len(got) != 1 || got[0] != declaredAlias {
		t.Fatalf("aliases = %v, want [%s]", got, declaredAlias)
	}
	if requestIndexContaining(requests, "DELETE ", groupOldEmail) < 0 {
		t.Fatalf("old primary alias was not deleted: %v", requests)
	}
	if requestIndexContaining(requests, "DELETE ", declaredAlias) >= 0 {
		t.Fatalf("declared alias was deleted: %v", requests)
	}
}

func TestResourceGroupUpdate_DelayedDeclaredAliasWrite(t *testing.T) {
	declaredAlias := "x@example.com"
	fake := newFakeAdmin(t)
	fake.aliasWritesAfterReads = 3
	fake.seedGroup(&directory.Group{Id: groupID, Email: groupOldEmail})
	state, result, requests, diags := applyGroupUpdateWithFake(t, fake, groupUpdateInput{
		oldEmail: groupOldEmail,
		config: map[string]interface{}{
			"email": groupNewEmail, "aliases": []interface{}{declaredAlias},
		},
		timeout: "15s",
	})
	if diags.HasError() {
		t.Fatalf("Apply failed: %v; requests: %v", diags, requests)
	}
	if state == nil || result == nil {
		t.Fatal("Apply returned no state")
	}
	if got := groupAliases(t, result); len(got) != 1 || got[0] != declaredAlias {
		t.Fatalf("aliases = %v, want [%s]", got, declaredAlias)
	}
	insert := requestIndex(requests, "POST /admin/directory/v1/groups/")
	readsAfterInsert := 0
	for i := insert + 1; i < len(requests); i++ {
		if strings.HasPrefix(requests[i], "GET /admin/directory/v1/groups/") {
			readsAfterInsert++
		}
	}
	if insert < 0 || readsAfterInsert < 6 {
		t.Fatalf("final wait must observe delayed alias and reach consistency: %v", requests)
	}
}

func TestResourceGroupUpdate_LeavesUnknownLiveAlias(t *testing.T) {
	unknownAlias := "unknown@example.com"
	declaredAlias := "x@example.com"
	fake := newFakeAdmin(t)
	fake.seedGroup(&directory.Group{Id: groupID, Email: groupOldEmail, Aliases: []string{unknownAlias}})
	_, result, requests, diags := applyGroupUpdateWithFake(t, fake, groupUpdateInput{
		oldEmail: groupOldEmail,
		config: map[string]interface{}{
			"email": groupNewEmail, "aliases": []interface{}{declaredAlias},
		},
		timeout: "5s",
	})
	if diags.HasError() {
		t.Fatalf("Apply failed: %v; requests: %v", diags, requests)
	}
	for _, want := range []string{unknownAlias, declaredAlias} {
		if !stringInSlice(groupAliases(t, result), want) {
			t.Errorf("aliases %v do not contain %s", groupAliases(t, result), want)
		}
	}
	if requestIndexContaining(requests, "DELETE ", unknownAlias) >= 0 {
		t.Fatalf("unknown live alias was deleted: %v", requests)
	}
}

func TestResourceGroupUpdate_DeclaredNonEditableAliasFails(t *testing.T) {
	domainAlias := "group@alias-domain.example.com"
	fake := newFakeAdmin(t)
	fake.seedGroup(&directory.Group{Id: groupID, Email: groupOldEmail, NonEditableAliases: []string{domainAlias}})
	_, _, requests, diags := applyGroupUpdateWithFake(t, fake, groupUpdateInput{
		oldEmail: groupOldEmail,
		config: map[string]interface{}{
			"email": groupNewEmail, "aliases": []interface{}{domainAlias},
		},
		timeout: "5s",
	})
	if !diags.HasError() || !strings.Contains(fmt.Sprint(diags), domainAlias) {
		t.Fatalf("Apply diagnostics %v do not name non-editable alias %s", diags, domainAlias)
	}
	if got := fakeAdminCountRequests(requests, "POST "); got != 0 {
		t.Fatalf("alias inserts = %d, want 0: %v", got, requests)
	}
}

func TestResourceGroupUpdate_NameOnlyKeepsMissingAliasOutOfWait(t *testing.T) {
	missingAlias := "missing@example.com"
	fake := newFakeAdmin(t)
	fake.seedGroup(&directory.Group{Id: groupID, Email: groupOldEmail})
	_, result, requests, diags := applyGroupUpdateWithFake(t, fake, groupUpdateInput{
		oldEmail:     groupOldEmail,
		stateAliases: []string{missingAlias},
		config: map[string]interface{}{
			"email": groupOldEmail, "aliases": []interface{}{missingAlias}, "name": "Updated name",
		},
		timeout: "3s",
	})
	if diags.HasError() {
		t.Fatalf("Apply failed: %v; requests: %v", diags, requests)
	}
	if result == nil {
		t.Fatal("Apply returned no state")
	}
	if got := result.Attributes["name"]; got != "Updated name" {
		t.Fatalf("name = %q, want Updated name", got)
	}
}

type groupUpdateInput struct {
	oldEmail     string
	stateAliases []string
	config       map[string]interface{}
	timeout      string
}

func applyGroupUpdateWithFake(
	t *testing.T,
	fake *fakeAdmin,
	input groupUpdateInput,
) (*terraform.InstanceState, *terraform.InstanceState, []string, diag.Diagnostics) {
	t.Helper()
	previousInterval := retryPollInterval
	retryPollInterval = time.Millisecond
	t.Cleanup(func() { retryPollInterval = previousInterval })
	client := newFakeAdminClient(t, fake)
	resource := resourceGroup()
	stateData := schema.TestResourceDataRaw(t, resource.Schema, map[string]interface{}{
		"email":   input.oldEmail,
		"aliases": stringSliceToInterfaces(input.stateAliases),
	})
	stateData.SetId(groupID)
	state := stateData.State()
	input.config["timeouts"] = map[string]interface{}{"update": input.timeout}
	diff, err := resource.Diff(context.Background(), state, terraform.NewResourceConfigRaw(input.config), client)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff == nil {
		t.Fatal("Diff returned nil")
	}
	result, diags := resource.Apply(context.Background(), state, diff, client)
	return state, result, fake.requestLog(), diags
}

func applyGroupUpdate(
	t *testing.T,
	seed directory.Group,
	config map[string]interface{},
	autoAliasReads int,
	updateTimeout string,
) (*terraform.InstanceState, *terraform.InstanceState, []string, diag.Diagnostics) {
	fake := newFakeAdmin(t)
	fake.autoAliasAfterReads = autoAliasReads
	fake.seedGroup(&seed)
	return applyGroupUpdateWithFake(t, fake, groupUpdateInput{
		oldEmail:     seed.Email,
		stateAliases: seed.Aliases,
		config:       config,
		timeout:      updateTimeout,
	})
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
		if (strings.HasPrefix(request, "POST ") && strings.HasSuffix(request, "/aliases")) ||
			(strings.HasPrefix(request, "DELETE ") && strings.Contains(request, "/aliases/")) {
			return i
		}
	}
	return -1
}

func urlPathEscape(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}
