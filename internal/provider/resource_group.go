package googleworkspace

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"

	directory "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/googleapi"
)

func resourceGroup() *schema.Resource {
	return &schema.Resource{
		// This description is used by the documentation generator and the language server.
		Description: "Group resource manages Google Workspace Groups. Group resides under the " +
			"`https://www.googleapis.com/auth/admin.directory.group` client scope.",

		CreateContext: resourceGroupCreate,
		ReadContext:   resourceGroupRead,
		UpdateContext: resourceGroupUpdate,
		DeleteContext: resourceGroupDelete,

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(10 * time.Minute),
			Update: schema.DefaultTimeout(10 * time.Minute),
		},

		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},

		Schema: map[string]*schema.Schema{
			"id": {
				Description: "The unique ID of a group. A group id can be used as a group request URI's groupKey.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"email": {
				Description: "The group's email address. If your account has multiple domains," +
					"select the appropriate domain for the email address. The email must be unique.",
				Type:     schema.TypeString,
				Required: true,
			},
			"name": {
				Description: "The group's display name.",
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
			},
			"description": {
				Description: "An extended description to help users determine the purpose of a group." +
					"For example, you can include information about who should join the group," +
					"the types of messages to send to the group, links to FAQs about the group, or related groups.",
				Type:             schema.TypeString,
				Optional:         true,
				ValidateDiagFunc: validation.ToDiagFunc(validation.StringLenBetween(0, 4096)),
			},
			"admin_created": {
				Description: "Value is true if this group was created by an administrator rather than a user.",
				Type:        schema.TypeBool,
				Computed:    true,
			},
			"direct_members_count": {
				Description: "The number of users that are direct members of the group." +
					"If a group is a member (child) of this group (the parent)," +
					"members of the child group are not counted in the directMembersCount property of the parent group.",
				Type:     schema.TypeInt,
				Computed: true,
			},
			"etag": {
				Description: "ETag of the resource.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"aliases": {
				Description: "asps.list of group's email addresses.",
				Type:        schema.TypeList,
				Optional:    true,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
			},
			"non_editable_aliases": {
				Description: "asps.list of the group's non-editable alias email addresses that are outside of the " +
					"account's primary domain or subdomains. These are functioning email addresses used by the group.",
				Type:     schema.TypeList,
				Computed: true,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
			},
		},
	}
}

func resourceGroupCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics

	// use the meta value to retrieve your client from the provider configure method
	client := meta.(*apiClient)

	email := d.Get("email").(string)
	log.Printf("[DEBUG] Creating Group %q: %#v", email, email)

	directoryService, diags := client.NewDirectoryService()
	if diags.HasError() {
		return diags
	}

	groupsService, diags := GetGroupsService(directoryService)
	if diags.HasError() {
		return diags
	}

	groupObj := directory.Group{
		Email:       d.Get("email").(string),
		Name:        d.Get("name").(string),
		Description: d.Get("description").(string),
	}

	group, err := groupsService.Insert(&groupObj).Do()
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId(group.Id)

	// INSERT will respond with the Group that will be created, however, it is eventually consistent.
	// After INSERT, the etag is updated along with the Group, and once we get a consistent etag we can
	// feel confident that our Group is also consistent. We wait for the GROUP to be consistent BEFORE
	// inserting any aliases: inserting an alias against a not-yet-propagated group 403s (the create-time
	// alias race). numInserts is a literal 1 here — the group alone.
	cc := consistencyCheck{
		resourceType: "group",
		timeout:      d.Timeout(schema.TimeoutCreate),
	}
	err = retryTimeDuration(ctx, d.Timeout(schema.TimeoutCreate), func() error {
		var retryErr error

		if cc.reachedConsistency(1) {
			return nil
		}

		newGroup, retryErr := groupsService.Get(d.Id()).IfNoneMatch(cc.lastEtag).Do()
		if googleapi.IsNotModified(retryErr) {
			cc.currConsistent += 1
		} else if isNotFound(retryErr) {
			// group was not found yet therefore setting currConsistent back to null value
			cc.currConsistent = 0
		} else if retryErr != nil {
			return fmt.Errorf("unexpected error during retries of %s: %s", cc.resourceType, retryErr)
		} else {
			cc.handleNewEtag(newGroup.Etag)
		}

		return fmt.Errorf("timed out while waiting for %s to be inserted", cc.resourceType)
	})

	if err != nil {
		return diag.FromErr(err)
	}

	// Now that the group is consistent, insert any aliases and wait for THEM to be consistent.
	aliases := d.Get("aliases.#").(int)

	if aliases > 0 {
		aliasesService, diags := GetGroupAliasService(groupsService)
		if diags.HasError() {
			return diags
		}

		aliasInserts := 0
		for i := 0; i < aliases; i++ {
			aliasObj := directory.Alias{
				Alias: d.Get(fmt.Sprintf("aliases.%d", i)).(string),
			}

			_, err := aliasesService.Insert(d.Id(), &aliasObj).Do()
			if err != nil {
				return diag.FromErr(err)
			}
			aliasInserts += 1
		}

		// The alias inserts are also eventually consistent. This MUST use a FRESH consistencyCheck:
		// reusing the group wait's cc leaves currConsistent==numConsistent, so
		// reachedConsistency(aliasInserts) would return true on the very first poll and the second
		// wait becomes a silent no-op — the race survives. A fresh cc with numInserts = the alias
		// count is what makes this wait real. It lives inside `if aliases > 0` so alias-less groups
		// pay zero extra polls.
		aliasCC := consistencyCheck{
			resourceType: "group",
			timeout:      d.Timeout(schema.TimeoutCreate),
		}
		err = retryTimeDuration(ctx, d.Timeout(schema.TimeoutCreate), func() error {
			var retryErr error

			if aliasCC.reachedConsistency(aliasInserts) {
				return nil
			}

			newGroup, retryErr := groupsService.Get(d.Id()).IfNoneMatch(aliasCC.lastEtag).Do()
			if googleapi.IsNotModified(retryErr) {
				aliasCC.currConsistent += 1
			} else if isNotFound(retryErr) {
				aliasCC.currConsistent = 0
			} else if retryErr != nil {
				return fmt.Errorf("unexpected error during retries of %s: %s", aliasCC.resourceType, retryErr)
			} else {
				aliasCC.handleNewEtag(newGroup.Etag)
			}

			return fmt.Errorf("timed out while waiting for %s aliases to be inserted", aliasCC.resourceType)
		})

		if err != nil {
			return diag.FromErr(err)
		}
	}

	log.Printf("[DEBUG] Finished creating Group %q: %#v", d.Id(), email)

	return resourceGroupRead(ctx, d, meta)
}

func resourceGroupRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics

	// use the meta value to retrieve your client from the provider configure method
	client := meta.(*apiClient)

	directoryService, diags := client.NewDirectoryService()
	if diags.HasError() {
		return diags
	}

	groupsService, diags := GetGroupsService(directoryService)
	if diags.HasError() {
		return diags
	}

	group, err := groupsService.Get(d.Id()).Do()
	if err != nil {
		return handleNotFoundError(err, d, d.Get("email").(string))
	}

	d.Set("email", group.Email)
	d.Set("name", group.Name)
	d.Set("description", group.Description)
	d.Set("admin_created", group.AdminCreated)
	d.Set("direct_members_count", group.DirectMembersCount)
	d.Set("aliases", group.Aliases)
	d.Set("non_editable_aliases", group.NonEditableAliases)
	d.Set("etag", group.Etag)

	d.SetId(group.Id)

	return diags
}

func resourceGroupUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics
	client := meta.(*apiClient)

	email := d.Get("email").(string)
	log.Printf("[DEBUG] Updating Group %q: %#v", d.Id(), email)

	directoryService, diags := client.NewDirectoryService()
	if diags.HasError() {
		return diags
	}

	groupsService, diags := GetGroupsService(directoryService)
	if diags.HasError() {
		return diags
	}

	aliasesService, diags := GetGroupAliasService(groupsService)
	if diags.HasError() {
		return diags
	}

	oldEmailValue, _ := d.GetChange("email")
	oldEmail := oldEmailValue.(string)
	emailChanged := d.HasChange("email")
	aliasesChanged := d.HasChange("aliases")
	var oldAliases, newAliases []string
	oldAliasValue, newAliasValue := d.GetChange("aliases")
	if aliasesChanged {
		oldAliases = listOfInterfacestoStrings(oldAliasValue)
		newAliases = listOfInterfacestoStrings(newAliasValue)
	}

	if emailChanged {
		live, err := groupsService.Get(d.Id()).Do()
		if err != nil {
			return diag.FromErr(err)
		}
		if stringInSlice(live.Aliases, email) {
			if err := aliasesService.Delete(d.Id(), email).Do(); err != nil {
				return diag.FromErr(err)
			}
			if err := waitForGroupAddresses(ctx, groupsService, d.Id(), d.Timeout(schema.TimeoutUpdate), nil, []string{email}); err != nil {
				return diag.FromErr(err)
			}
		} else if stringInSlice(live.NonEditableAliases, email) {
			return diag.Errorf("cannot promote non-editable group alias %q to primary email", email)
		}
	}

	groupObj := directory.Group{}
	if emailChanged {
		groupObj.Email = email
	}
	if d.HasChange("name") {
		groupObj.Name = d.Get("name").(string)
	}
	if d.HasChange("description") {
		groupObj.Description = d.Get("description").(string)
	}
	if &groupObj != new(directory.Group) {
		group, err := groupsService.Update(d.Id(), &groupObj).Do()
		if err != nil {
			return diag.FromErr(err)
		}
		d.SetId(group.Id)
	}

	if emailChanged {
		if err := waitForGroupAddresses(ctx, groupsService, d.Id(), d.Timeout(schema.TimeoutUpdate), []string{oldEmail}, nil); err != nil {
			return diag.FromErr(err)
		}
	}

	if emailChanged || aliasesChanged {
		live, err := groupsService.Get(d.Id()).Do()
		if err != nil {
			return diag.FromErr(err)
		}
		declared := make(map[string]struct{}, len(newAliases))
		for _, alias := range newAliases {
			declared[alias] = struct{}{}
		}
		toDelete := make([]string, 0, len(oldAliases)+1)
		for _, alias := range oldAliases {
			if _, ok := declared[alias]; !ok && stringInSlice(live.Aliases, alias) {
				toDelete = append(toDelete, alias)
			}
		}
		if emailChanged {
			if _, ok := declared[oldEmail]; !ok && stringInSlice(live.Aliases, oldEmail) {
				toDelete = append(toDelete, oldEmail)
			}
		}
		for _, alias := range toDelete {
			if err := aliasesService.Delete(d.Id(), alias).Do(); err != nil {
				return diag.FromErr(err)
			}
		}
		for _, alias := range newAliases {
			if !stringInSlice(live.Aliases, alias) && alias != oldEmail {
				aliasObj := directory.Alias{Alias: alias}
				if _, err := aliasesService.Insert(d.Id(), &aliasObj).Do(); err != nil {
					return diag.FromErr(err)
				}
			}
		}
	}

	if err := waitForGroupAddresses(ctx, groupsService, d.Id(), d.Timeout(schema.TimeoutUpdate), nil, nil); err != nil {
		return diag.FromErr(err)
	}

	log.Printf("[DEBUG] Finished updating Group %q: %#v", d.Id(), email)
	return resourceGroupRead(ctx, d, meta)
}

func groupAddresses(group *directory.Group) []string {
	addresses := make([]string, 0, len(group.Aliases)+len(group.NonEditableAliases))
	addresses = append(addresses, group.Aliases...)
	addresses = append(addresses, group.NonEditableAliases...)
	return addresses
}

func waitForGroupAddresses(
	ctx context.Context,
	groupsService *directory.GroupsService,
	id string,
	timeout time.Duration,
	present, absent []string,
) error {
	var missing, extra []string
	cc := consistencyCheck{resourceType: "group", timeout: timeout}
	addressesConsistent := false
	return retryTimeDuration(ctx, timeout, func() error {
		group, retryErr := groupsService.Get(id).IfNoneMatch(cc.lastEtag).Do()
		if googleapi.IsNotModified(retryErr) {
			if addressesConsistent {
				cc.currConsistent++
			} else {
				cc.currConsistent = 0
			}
		} else if retryErr != nil {
			return fmt.Errorf("unexpected error while checking group addresses: %w", retryErr)
		} else {
			cc.handleNewEtag(group.Etag)
			addresses := groupAddresses(group)
			missing = missingAddresses(addresses, present)
			extra = extraAddresses(addresses, absent)
			addressesConsistent = len(missing) == 0 && len(extra) == 0
			if addressesConsistent {
				cc.currConsistent++
			}
		}
		if cc.currConsistent >= numConsistent {
			return nil
		}
		return fmt.Errorf("timed out while waiting for group addresses: missing %v, extra %v", missing, extra)
	})
}

func missingAddresses(addresses, present []string) []string {
	var missing []string
	for _, address := range present {
		if !stringInSlice(addresses, address) {
			missing = append(missing, address)
		}
	}
	sort.Strings(missing)
	return missing
}

func extraAddresses(addresses, absent []string) []string {
	var extra []string
	for _, address := range absent {
		if stringInSlice(addresses, address) {
			extra = append(extra, address)
		}
	}
	sort.Strings(extra)
	return extra
}

func resourceGroupDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var diags diag.Diagnostics

	// use the meta value to retrieve your client from the provider configure method
	client := meta.(*apiClient)

	email := d.Get("email").(string)
	log.Printf("[DEBUG] Deleting Group %q: %#v", d.Id(), email)

	directoryService, diags := client.NewDirectoryService()
	if diags.HasError() {
		return diags
	}

	groupsService, diags := GetGroupsService(directoryService)
	if diags.HasError() {
		return diags
	}

	err := groupsService.Delete(d.Id()).Do()
	if err != nil {
		return handleNotFoundError(err, d, d.Get("email").(string))
	}

	log.Printf("[DEBUG] Finished deleting Group %q: %#v", d.Id(), email)

	return diags
}
