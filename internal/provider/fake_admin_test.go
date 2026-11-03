package googleworkspace

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	directory "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/groupssettings/v1"
)

type fakeAdmin struct {
	server *httptest.Server
	t      *testing.T
	mu     sync.Mutex

	groups                      map[string]*fakeAdminGroup
	requests                    []string
	autoAliasAfterReads         int
	aliasWritesAfterReads       int
	settingsFailuresAfterRename int
	settingsFailures            map[string]int
	settingsErrors              map[string]fakeAdminError
	settings                    map[string]*groupssettings.Groups
	settingsEtags               map[string]string
	settingsPutBodies           map[string]map[string]json.RawMessage
}

type fakeAdminGroup struct {
	group                   directory.Group
	pendingAutoAlias        string
	remainingAutoAliasReads int
	pendingAliasWrites      map[string]*fakeAdminAliasWrite
}

type fakeAdminAliasWrite struct {
	present        bool
	remainingReads int
}

type fakeAdminError struct {
	code    int
	reason  string
	message string
}

func newFakeAdmin(t *testing.T) *fakeAdmin {
	f := &fakeAdmin{
		t:                 t,
		groups:            make(map[string]*fakeAdminGroup),
		settingsFailures:  make(map[string]int),
		settingsErrors:    make(map[string]fakeAdminError),
		settings:          make(map[string]*groupssettings.Groups),
		settingsEtags:     make(map[string]string),
		settingsPutBodies: make(map[string]map[string]json.RawMessage),
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.close)
	return f
}

func (f *fakeAdmin) close() {
	f.server.Close()
}

func (f *fakeAdmin) seedGroup(group *directory.Group) {
	f.mu.Lock()
	defer f.mu.Unlock()

	copy := *group
	copy.Email = strings.ToLower(copy.Email)
	copy.Aliases = append([]string(nil), group.Aliases...)
	for i, alias := range copy.Aliases {
		copy.Aliases[i] = strings.ToLower(alias)
	}
	copy.NonEditableAliases = append([]string(nil), group.NonEditableAliases...)
	for i, alias := range copy.NonEditableAliases {
		copy.NonEditableAliases[i] = strings.ToLower(alias)
	}
	if copy.Etag == "" {
		copy.Etag = "etag-1"
	}
	f.groups[copy.Id] = &fakeAdminGroup{group: copy, pendingAliasWrites: make(map[string]*fakeAdminAliasWrite)}
	f.settings[copy.Email] = fakeAdminDefaultSettings(copy.Email)
	f.settingsEtags[copy.Email] = "settings-etag-1"
}

func (f *fakeAdmin) renameGroup(oldEmail, newEmail string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	group := f.findGroupLocked(oldEmail)
	if group == nil {
		return
	}
	newEmail = strings.ToLower(newEmail)
	group.group.Email = newEmail
	group.group.Etag = f.nextEtag(group.group.Etag)
	f.settings[newEmail] = fakeAdminDefaultSettings(newEmail)
	f.settingsEtags[newEmail] = "settings-etag-1"
	if f.settingsFailuresAfterRename > 0 {
		f.settingsFailures[newEmail] = f.settingsFailuresAfterRename
	}
}

func (f *fakeAdmin) setSettingsError(email string, code int, reason, message string, count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settingsErrors[email] = fakeAdminError{code: code, reason: reason, message: message}
	f.settingsFailures[email] = count
}

func (f *fakeAdmin) requestLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func (f *fakeAdmin) settingsPutBody(email string) map[string]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	body := make(map[string]json.RawMessage, len(f.settingsPutBodies[email]))
	for key, value := range f.settingsPutBodies[email] {
		body[key] = append(json.RawMessage(nil), value...)
	}
	return body
}

func (f *fakeAdmin) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
	path := strings.Trim(r.URL.Path, "/")
	const directoryPrefix = "admin/directory/v1/groups/"
	const settingsPrefix = "groups/v1/groups/"

	switch {
	case strings.HasPrefix(path, directoryPrefix):
		f.serveDirectory(w, r, strings.TrimPrefix(path, directoryPrefix))
	case strings.HasPrefix(path, settingsPrefix):
		f.serveSettings(w, r, strings.TrimPrefix(path, settingsPrefix))
	default:
		f.writeError(w, http.StatusNotFound, "notFound", "route not found")
	}
}

func (f *fakeAdmin) serveDirectory(w http.ResponseWriter, r *http.Request, route string) {
	parts := strings.Split(route, "/")
	group := f.findGroupLocked(parts[0])
	if group == nil {
		f.writeError(w, http.StatusNotFound, "notFound", "group not found")
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			f.serveGroupGet(w, r, group)
		case http.MethodPatch, http.MethodPut:
			f.serveGroupPatch(w, r, group)
		default:
			f.writeError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
		}
		return
	}
	if len(parts) == 2 && parts[1] == "aliases" && r.Method == http.MethodPost {
		f.serveAliasInsert(w, r, group)
		return
	}
	if len(parts) == 3 && parts[1] == "aliases" && r.Method == http.MethodDelete {
		f.serveAliasDelete(w, group, parts[2])
		return
	}

	f.writeError(w, http.StatusNotFound, "notFound", "route not found")
}

func (f *fakeAdmin) serveGroupGet(w http.ResponseWriter, r *http.Request, group *fakeAdminGroup) {
	if group.pendingAutoAlias != "" && group.remainingAutoAliasReads > 0 {
		group.remainingAutoAliasReads--
		if group.remainingAutoAliasReads == 0 {
			group.group.Aliases = append(group.group.Aliases, group.pendingAutoAlias)
			group.pendingAutoAlias = ""
			group.group.Etag = f.nextEtag(group.group.Etag)
		}
	}
	for alias, pending := range group.pendingAliasWrites {
		if pending.remainingReads > 0 {
			pending.remainingReads--
		}
		if pending.remainingReads == 0 {
			if pending.present {
				group.group.Aliases = append(group.group.Aliases, alias)
			} else {
				for i, current := range group.group.Aliases {
					if strings.EqualFold(current, alias) {
						group.group.Aliases = append(group.group.Aliases[:i], group.group.Aliases[i+1:]...)
						break
					}
				}
			}
			delete(group.pendingAliasWrites, alias)
			group.group.Etag = f.nextEtag(group.group.Etag)
		}
	}

	w.Header().Set("ETag", group.group.Etag)
	if r.Header.Get("If-None-Match") == group.group.Etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	f.writeJSON(w, http.StatusOK, &group.group)
}

func (f *fakeAdmin) serveGroupPatch(w http.ResponseWriter, r *http.Request, group *fakeAdminGroup) {
	var update directory.Group
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		f.writeError(w, http.StatusBadRequest, "invalid", "invalid group body")
		return
	}

	oldEmail := group.group.Email
	if update.Email != "" && !strings.EqualFold(update.Email, oldEmail) {
		if fakeAdminAddressInSlice(group.group.Aliases, update.Email) ||
			fakeAdminAddressInSlice(group.group.NonEditableAliases, update.Email) {
			f.writeError(w, http.StatusBadRequest, "invalid", "Invalid Input: email is held as an alias")
			return
		}
		group.group.Email = strings.ToLower(update.Email)
		if f.autoAliasAfterReads == 0 {
			group.group.Aliases = append(group.group.Aliases, oldEmail)
		} else if f.autoAliasAfterReads > 0 {
			group.pendingAutoAlias = oldEmail
			group.remainingAutoAliasReads = f.autoAliasAfterReads
		}
	}
	if update.Name != "" {
		group.group.Name = update.Name
	}
	if update.Description != "" {
		group.group.Description = update.Description
	}
	group.group.Etag = f.nextEtag(group.group.Etag)
	w.Header().Set("ETag", group.group.Etag)
	f.writeJSON(w, http.StatusOK, &group.group)
}

func (f *fakeAdmin) serveAliasDelete(w http.ResponseWriter, group *fakeAdminGroup, alias string) {
	for i, current := range group.group.Aliases {
		if strings.EqualFold(current, alias) {
			group.group.Aliases = append(group.group.Aliases[:i], group.group.Aliases[i+1:]...)
			group.group.Etag = f.nextEtag(group.group.Etag)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	f.writeError(w, http.StatusNotFound, "notFound", "alias not found")
}

func (f *fakeAdmin) serveAliasInsert(w http.ResponseWriter, r *http.Request, group *fakeAdminGroup) {
	var alias directory.Alias
	if err := json.NewDecoder(r.Body).Decode(&alias); err != nil {
		f.writeError(w, http.StatusBadRequest, "invalid", "invalid alias body")
		return
	}
	alias.Alias = strings.ToLower(alias.Alias)
	pending := group.pendingAliasWrites[alias.Alias]
	isPrimary := strings.EqualFold(alias.Alias, group.group.Email)
	isHeld := fakeAdminAddressInSlice(group.group.Aliases, alias.Alias) ||
		fakeAdminAddressInSlice(group.group.NonEditableAliases, alias.Alias)
	isPending := pending != nil && pending.present
	if isPrimary || isHeld || isPending {
		f.writeError(w, http.StatusConflict, "duplicate", "duplicate alias")
		return
	}
	if f.aliasWritesAfterReads > 0 {
		group.pendingAliasWrites[alias.Alias] = &fakeAdminAliasWrite{
			present:        true,
			remainingReads: f.aliasWritesAfterReads,
		}
		w.Header().Set("ETag", group.group.Etag)
		f.writeJSON(w, http.StatusOK, &alias)
		return
	}
	group.group.Aliases = append(group.group.Aliases, alias.Alias)
	group.group.Etag = f.nextEtag(group.group.Etag)
	alias.Etag = group.group.Etag
	alias.PrimaryEmail = group.group.Email
	w.Header().Set("ETag", group.group.Etag)
	f.writeJSON(w, http.StatusOK, &alias)
}

func (f *fakeAdmin) serveSettings(w http.ResponseWriter, r *http.Request, email string) {
	if r.Method == http.MethodGet {
		settings := f.settings[email]
		if settings == nil {
			f.writeError(w, http.StatusNotFound, "notFound", "group settings not found")
			return
		}
		etag := f.settingsEtags[email]
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		f.writeJSON(w, http.StatusOK, settings)
		return
	}
	if r.Method != http.MethodPut {
		f.writeError(w, http.StatusMethodNotAllowed, "invalid", "method not allowed")
		return
	}

	if f.settingsFailures[email] > 0 {
		f.settingsFailures[email]--
		apiError, ok := f.settingsErrors[email]
		if !ok {
			apiError = fakeAdminError{
				code:    http.StatusBadRequest,
				reason:  "required",
				message: "Required, required",
			}
		}
		f.writeError(w, apiError.code, apiError.reason, apiError.message)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.writeError(w, http.StatusBadRequest, "invalid", "invalid settings body")
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		f.writeError(w, http.StatusBadRequest, "invalid", "invalid settings body")
		return
	}
	if len(fields) == 0 {
		f.writeError(w, http.StatusBadRequest, "required", "Required, required")
		return
	}
	var update groupssettings.Groups
	if err := json.Unmarshal(body, &update); err != nil {
		f.writeError(w, http.StatusBadRequest, "invalid", "invalid settings body")
		return
	}
	f.settingsPutBodies[email] = fields
	settings := f.settings[email]
	if settings == nil {
		settings = fakeAdminDefaultSettings(email)
	}
	update.Email = email
	update.AllowExternalMembers = fakeAdminValue(update.AllowExternalMembers, settings.AllowExternalMembers)
	update.AllowWebPosting = fakeAdminValue(update.AllowWebPosting, settings.AllowWebPosting)
	update.IsArchived = fakeAdminValue(update.IsArchived, settings.IsArchived)
	update.ArchiveOnly = fakeAdminValue(update.ArchiveOnly, settings.ArchiveOnly)
	update.IncludeCustomFooter = fakeAdminValue(update.IncludeCustomFooter, settings.IncludeCustomFooter)
	update.SendMessageDenyNotification = fakeAdminValue(
		update.SendMessageDenyNotification, settings.SendMessageDenyNotification,
	)
	update.MembersCanPostAsTheGroup = fakeAdminValue(
		update.MembersCanPostAsTheGroup, settings.MembersCanPostAsTheGroup,
	)
	update.IncludeInGlobalAddressList = fakeAdminValue(
		update.IncludeInGlobalAddressList, settings.IncludeInGlobalAddressList,
	)
	update.CustomRolesEnabledForSettingsToBeMerged = fakeAdminValue(
		update.CustomRolesEnabledForSettingsToBeMerged,
		settings.CustomRolesEnabledForSettingsToBeMerged,
	)
	update.EnableCollaborativeInbox = fakeAdminValue(
		update.EnableCollaborativeInbox, settings.EnableCollaborativeInbox,
	)
	f.settings[email] = &update
	f.settingsEtags[email] = f.nextSettingsEtag(f.settingsEtags[email])
	w.Header().Set("ETag", f.settingsEtags[email])
	f.writeJSON(w, http.StatusOK, &update)
}

func (f *fakeAdmin) findGroupLocked(key string) *fakeAdminGroup {
	if group := f.groups[key]; group != nil {
		return group
	}
	for _, group := range f.groups {
		if strings.EqualFold(group.group.Email, key) || fakeAdminAddressInSlice(group.group.Aliases, key) ||
			fakeAdminAddressInSlice(group.group.NonEditableAliases, key) {
			return group
		}
	}
	return nil
}

func fakeAdminAddressInSlice(addresses []string, address string) bool {
	for _, candidate := range addresses {
		if strings.EqualFold(candidate, address) {
			return true
		}
	}
	return false
}

func (f *fakeAdmin) nextEtag(etag string) string {
	var version int
	if _, err := fmt.Sscanf(etag, "etag-%d", &version); err != nil {
		version = 0
	}
	return fmt.Sprintf("etag-%d", version+1)
}

func (f *fakeAdmin) nextSettingsEtag(etag string) string {
	var version int
	if _, err := fmt.Sscanf(etag, "settings-etag-%d", &version); err != nil {
		version = 0
	}
	return fmt.Sprintf("settings-etag-%d", version+1)
}

func (f *fakeAdmin) writeError(w http.ResponseWriter, code int, reason, message string) {
	f.writeJSON(w, code, map[string]interface{}{
		"error": map[string]interface{}{
			"code":    code,
			"message": message,
			"errors": []map[string]string{{
				"domain":  "global",
				"message": message,
				"reason":  reason,
			}},
		},
	})
}

func (f *fakeAdmin) writeJSON(w http.ResponseWriter, code int, value interface{}) {
	body, err := json.Marshal(value)
	if err != nil {
		f.t.Errorf("encode fake Admin response: %v", err)
		http.Error(w, "fake Admin response encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if _, err := w.Write(append(body, '\n')); err != nil {
		f.t.Errorf("write fake Admin response: %v", err)
	}
}

func fakeAdminDefaultSettings(email string) *groupssettings.Groups {
	return &groupssettings.Groups{
		Email:                                   email,
		AllowExternalMembers:                    "false",
		AllowWebPosting:                         "false",
		IsArchived:                              "false",
		ArchiveOnly:                             "false",
		IncludeCustomFooter:                     "false",
		SendMessageDenyNotification:             "false",
		MembersCanPostAsTheGroup:                "false",
		IncludeInGlobalAddressList:              "false",
		CustomRolesEnabledForSettingsToBeMerged: "false",
		EnableCollaborativeInbox:                "false",
	}
}

func fakeAdminValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

type fakeAdminRoundTripper struct {
	server    *url.URL
	transport http.RoundTripper
}

func (t *fakeAdminRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	requestURL := *request.URL
	requestURL.Scheme = t.server.Scheme
	requestURL.Host = t.server.Host
	copy.URL = &requestURL
	copy.Host = t.server.Host
	return t.transport.RoundTrip(copy)
}

func newFakeAdminClient(t *testing.T, f *fakeAdmin) *apiClient {
	t.Helper()

	serverURL, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatalf("parse fake Admin URL: %v", err)
	}
	client := &http.Client{Transport: &fakeAdminRoundTripper{
		server:    serverURL,
		transport: http.DefaultTransport,
	}}
	return &apiClient{client: client}
}
