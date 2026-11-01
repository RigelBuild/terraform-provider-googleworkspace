package googleworkspace

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	directory "google.golang.org/api/admin/directory/v1"
)

func TestResourceGroupSettingsUpdate_EmailPropagation(t *testing.T) {
	_, _, requests, diags := applyGroupSettingsUpdate(t, true, 2, "5s")
	if diags.HasError() {
		t.Fatalf("Apply failed: %v; requests: %v", diags, requests)
	}
	if got := fakeAdminCountRequests(requests, "PUT /groups/v1/groups/"); got != 3 {
		t.Fatalf("settings PUTs = %d, want 3", got)
	}
}

func TestResourceGroupSettingsUpdate_OtherErrorFailsFast(t *testing.T) {
	fake := newFakeAdmin(t)
	fake.seedGroup(&directory.Group{Id: groupID, Email: groupOldEmail})
	fake.setSettingsError(groupNewEmail, 400, "invalid", "Invalid Input", 10)
	_, requests, diags := applyGroupSettingsUpdateWithFake(t, fake, true, "5s")
	if !diags.HasError() {
		t.Fatal("Apply succeeded after a non-propagation error")
	}
	if got := fakeAdminCountRequests(requests, "PUT /groups/v1/groups/"); got != 1 {
		t.Fatalf("settings PUTs = %d, want 1", got)
	}
}

func TestResourceGroupSettingsUpdate_RequiredWithoutEmailChangeFailsFast(t *testing.T) {
	fake := newFakeAdmin(t)
	fake.seedGroup(&directory.Group{Id: groupID, Email: groupNewEmail})
	fake.setSettingsError(groupNewEmail, 400, "required", "Required, required", 10)
	_, requests, diags := applyGroupSettingsUpdateWithFake(t, fake, false, "5s")
	if !diags.HasError() {
		t.Fatal("Apply succeeded after a required error without an email change")
	}
	if got := fakeAdminCountRequests(requests, "PUT /groups/v1/groups/"); got != 1 {
		t.Fatalf("settings PUTs = %d, want 1", got)
	}
}

func TestResourceGroupSettingsUpdate_PropagationTimeoutFailsLoud(t *testing.T) {
	_, _, requests, diags := applyGroupSettingsUpdate(t, true, 100, "100ms")
	if !diags.HasError() {
		t.Fatal("Apply succeeded before propagation timeout")
	}
	if !strings.Contains(fmt.Sprint(diags), "Required, required") {
		t.Fatalf("timeout error does not carry Google message: %v", diags)
	}
	if puts := fakeAdminCountRequests(requests, "PUT /groups/v1/groups/"); puts <= 1 {
		t.Fatalf("settings PUTs = %d, want retries before timeout", puts)
	}
}

func applyGroupSettingsUpdate(
	t *testing.T,
	emailChange bool,
	propagationFailures int,
	updateTimeout string,
) (*fakeAdmin, *terraform.InstanceState, []string, diag.Diagnostics) {
	t.Helper()
	previousInterval := retryPollIntervalOverride
	retryPollIntervalOverride = time.Millisecond
	t.Cleanup(func() { retryPollIntervalOverride = previousInterval })
	fake := newFakeAdmin(t)
	seedEmail := groupNewEmail
	if emailChange {
		seedEmail = groupOldEmail
	}
	fake.seedGroup(&directory.Group{Id: groupID, Email: seedEmail})
	fake.settingsFailuresAfterRename = propagationFailures
	state, requests, diags := applyGroupSettingsUpdateWithFake(t, fake, emailChange, updateTimeout)
	return fake, state, requests, diags
}

func applyGroupSettingsUpdateWithFake(
	t *testing.T,
	fake *fakeAdmin,
	emailChange bool,
	updateTimeout string,
) (*terraform.InstanceState, []string, diag.Diagnostics) {
	t.Helper()
	seedEmail := groupNewEmail
	if emailChange {
		seedEmail = groupOldEmail
		fake.renameGroup(seedEmail, groupNewEmail)
	}
	client := newFakeAdminClient(t, fake)
	resource := resourceGroupSettings()
	stateData := schema.TestResourceDataRaw(t, resource.Schema, map[string]interface{}{"email": seedEmail})
	stateData.SetId(seedEmail)
	state := stateData.State()
	config := map[string]interface{}{
		"email":    groupNewEmail,
		"timeouts": map[string]interface{}{"update": updateTimeout},
	}
	if !emailChange {
		config["email"] = seedEmail
		config["who_can_join"] = "ANYONE_CAN_JOIN"
	}
	diff, err := resource.Diff(context.Background(), state, terraform.NewResourceConfigRaw(config), client)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff == nil {
		t.Fatal("Diff returned nil")
	}
	result, diags := resource.Apply(context.Background(), state, diff, client)
	return result, fake.requestLog(), diags
}
