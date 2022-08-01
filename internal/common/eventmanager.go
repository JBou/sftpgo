// Copyright (C) 2019-2022  Nicola Murino
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, version 3.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package common

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/drakkan/sftpgo/v2/internal/dataprovider"
	"github.com/drakkan/sftpgo/v2/internal/logger"
	"github.com/drakkan/sftpgo/v2/internal/plugin"
	"github.com/drakkan/sftpgo/v2/internal/smtp"
	"github.com/drakkan/sftpgo/v2/internal/util"
	"github.com/drakkan/sftpgo/v2/internal/vfs"
)

var (
	// eventManager handle the supported event rules actions
	eventManager eventRulesContainer
)

func init() {
	eventManager = eventRulesContainer{
		schedulesMapping: make(map[string][]cron.EntryID),
	}
	dataprovider.SetEventRulesCallbacks(eventManager.loadRules, eventManager.RemoveRule,
		func(operation, executor, ip, objectType, objectName string, object plugin.Renderer) {
			eventManager.handleProviderEvent(EventParams{
				Name:       executor,
				ObjectName: objectName,
				Event:      operation,
				Status:     1,
				ObjectType: objectType,
				IP:         ip,
				Timestamp:  time.Now().UnixNano(),
				Object:     object,
			})
		})
}

// eventRulesContainer stores event rules by trigger
type eventRulesContainer struct {
	sync.RWMutex
	FsEvents         []dataprovider.EventRule
	ProviderEvents   []dataprovider.EventRule
	Schedules        []dataprovider.EventRule
	schedulesMapping map[string][]cron.EntryID
	lastLoad         int64
}

func (r *eventRulesContainer) getLastLoadTime() int64 {
	return atomic.LoadInt64(&r.lastLoad)
}

func (r *eventRulesContainer) setLastLoadTime(modTime int64) {
	atomic.StoreInt64(&r.lastLoad, modTime)
}

// RemoveRule deletes the rule with the specified name
func (r *eventRulesContainer) RemoveRule(name string) {
	r.Lock()
	defer r.Unlock()

	r.removeRuleInternal(name)
	eventManagerLog(logger.LevelDebug, "event rules updated after delete, fs events: %d, provider events: %d, schedules: %d",
		len(r.FsEvents), len(r.ProviderEvents), len(r.Schedules))
}

func (r *eventRulesContainer) removeRuleInternal(name string) {
	for idx := range r.FsEvents {
		if r.FsEvents[idx].Name == name {
			lastIdx := len(r.FsEvents) - 1
			r.FsEvents[idx] = r.FsEvents[lastIdx]
			r.FsEvents = r.FsEvents[:lastIdx]
			eventManagerLog(logger.LevelDebug, "removed rule %q from fs events", name)
			return
		}
	}
	for idx := range r.ProviderEvents {
		if r.ProviderEvents[idx].Name == name {
			lastIdx := len(r.ProviderEvents) - 1
			r.ProviderEvents[idx] = r.ProviderEvents[lastIdx]
			r.ProviderEvents = r.ProviderEvents[:lastIdx]
			eventManagerLog(logger.LevelDebug, "removed rule %q from provider events", name)
			return
		}
	}
	for idx := range r.Schedules {
		if r.Schedules[idx].Name == name {
			if schedules, ok := r.schedulesMapping[name]; ok {
				for _, entryID := range schedules {
					eventManagerLog(logger.LevelDebug, "removing scheduled entry id %d for rule %q", entryID, name)
					eventScheduler.Remove(entryID)
				}
				delete(r.schedulesMapping, name)
			}

			lastIdx := len(r.Schedules) - 1
			r.Schedules[idx] = r.Schedules[lastIdx]
			r.Schedules = r.Schedules[:lastIdx]
			eventManagerLog(logger.LevelDebug, "removed rule %q from scheduled events", name)
			return
		}
	}
}

func (r *eventRulesContainer) addUpdateRuleInternal(rule dataprovider.EventRule) {
	r.removeRuleInternal(rule.Name)
	if rule.DeletedAt > 0 {
		deletedAt := util.GetTimeFromMsecSinceEpoch(rule.DeletedAt)
		if deletedAt.Add(30 * time.Minute).Before(time.Now()) {
			eventManagerLog(logger.LevelDebug, "removing rule %q deleted at %s", rule.Name, deletedAt)
			go dataprovider.RemoveEventRule(rule) //nolint:errcheck
		}
		return
	}
	switch rule.Trigger {
	case dataprovider.EventTriggerFsEvent:
		r.FsEvents = append(r.FsEvents, rule)
		eventManagerLog(logger.LevelDebug, "added rule %q to fs events", rule.Name)
	case dataprovider.EventTriggerProviderEvent:
		r.ProviderEvents = append(r.ProviderEvents, rule)
		eventManagerLog(logger.LevelDebug, "added rule %q to provider events", rule.Name)
	case dataprovider.EventTriggerSchedule:
		for _, schedule := range rule.Conditions.Schedules {
			cronSpec := schedule.GetCronSpec()
			job := &eventCronJob{
				ruleName: dataprovider.ConvertName(rule.Name),
			}
			entryID, err := eventScheduler.AddJob(cronSpec, job)
			if err != nil {
				eventManagerLog(logger.LevelError, "unable to add scheduled rule %q, cron string %q: %v", rule.Name, cronSpec, err)
				return
			}
			r.schedulesMapping[rule.Name] = append(r.schedulesMapping[rule.Name], entryID)
			eventManagerLog(logger.LevelDebug, "schedule for rule %q added, id: %d, cron string %q, active scheduling rules: %d",
				rule.Name, entryID, cronSpec, len(r.schedulesMapping))
		}
		r.Schedules = append(r.Schedules, rule)
		eventManagerLog(logger.LevelDebug, "added rule %q to scheduled events", rule.Name)
	default:
		eventManagerLog(logger.LevelError, "unsupported trigger: %d", rule.Trigger)
	}
}

func (r *eventRulesContainer) loadRules() {
	eventManagerLog(logger.LevelDebug, "loading updated rules")
	modTime := util.GetTimeAsMsSinceEpoch(time.Now())
	rules, err := dataprovider.GetRecentlyUpdatedRules(r.getLastLoadTime())
	if err != nil {
		eventManagerLog(logger.LevelError, "unable to load event rules: %v", err)
		return
	}
	eventManagerLog(logger.LevelDebug, "recently updated event rules loaded: %d", len(rules))

	if len(rules) > 0 {
		r.Lock()
		defer r.Unlock()

		for _, rule := range rules {
			r.addUpdateRuleInternal(rule)
		}
	}
	eventManagerLog(logger.LevelDebug, "event rules updated, fs events: %d, provider events: %d, schedules: %d",
		len(r.FsEvents), len(r.ProviderEvents), len(r.Schedules))

	r.setLastLoadTime(modTime)
}

func (r *eventRulesContainer) checkProviderEventMatch(conditions dataprovider.EventConditions, params EventParams) bool {
	if !util.Contains(conditions.ProviderEvents, params.Event) {
		return false
	}
	if !checkEventConditionPatterns(params.Name, conditions.Options.Names) {
		return false
	}
	if len(conditions.Options.ProviderObjects) > 0 && !util.Contains(conditions.Options.ProviderObjects, params.ObjectType) {
		return false
	}
	return true
}

func (r *eventRulesContainer) checkFsEventMatch(conditions dataprovider.EventConditions, params EventParams) bool {
	if !util.Contains(conditions.FsEvents, params.Event) {
		return false
	}
	if !checkEventConditionPatterns(params.Name, conditions.Options.Names) {
		return false
	}
	if !checkEventConditionPatterns(params.VirtualPath, conditions.Options.FsPaths) {
		if !checkEventConditionPatterns(params.ObjectName, conditions.Options.FsPaths) {
			return false
		}
	}
	if len(conditions.Options.Protocols) > 0 && !util.Contains(conditions.Options.Protocols, params.Protocol) {
		return false
	}
	if params.Event == operationUpload || params.Event == operationDownload {
		if conditions.Options.MinFileSize > 0 {
			if params.FileSize < conditions.Options.MinFileSize {
				return false
			}
		}
		if conditions.Options.MaxFileSize > 0 {
			if params.FileSize > conditions.Options.MaxFileSize {
				return false
			}
		}
	}
	return true
}

// hasFsRules returns true if there are any rules for filesystem event triggers
func (r *eventRulesContainer) hasFsRules() bool {
	r.RLock()
	defer r.RUnlock()

	return len(r.FsEvents) > 0
}

// handleFsEvent executes the rules actions defined for the specified event
func (r *eventRulesContainer) handleFsEvent(params EventParams) error {
	r.RLock()

	var rulesWithSyncActions, rulesAsync []dataprovider.EventRule
	for _, rule := range r.FsEvents {
		if r.checkFsEventMatch(rule.Conditions, params) {
			hasSyncActions := false
			for _, action := range rule.Actions {
				if action.Options.ExecuteSync {
					hasSyncActions = true
					break
				}
			}
			if hasSyncActions {
				rulesWithSyncActions = append(rulesWithSyncActions, rule)
			} else {
				rulesAsync = append(rulesAsync, rule)
			}
		}
	}

	r.RUnlock()

	if len(rulesAsync) > 0 {
		go executeAsyncRulesActions(rulesAsync, params)
	}

	if len(rulesWithSyncActions) > 0 {
		return executeSyncRulesActions(rulesWithSyncActions, params)
	}
	return nil
}

func (r *eventRulesContainer) handleProviderEvent(params EventParams) {
	r.RLock()
	defer r.RUnlock()

	var rules []dataprovider.EventRule
	for _, rule := range r.ProviderEvents {
		if r.checkProviderEventMatch(rule.Conditions, params) {
			rules = append(rules, rule)
		}
	}

	if len(rules) > 0 {
		go executeAsyncRulesActions(rules, params)
	}
}

// EventParams defines the supported event parameters
type EventParams struct {
	Name              string
	Event             string
	Status            int
	VirtualPath       string
	FsPath            string
	VirtualTargetPath string
	FsTargetPath      string
	ObjectName        string
	ObjectType        string
	FileSize          int64
	Protocol          string
	IP                string
	Timestamp         int64
	Object            plugin.Renderer
}

func (p *EventParams) getStringReplacements(addObjectData bool) []string {
	replacements := []string{
		"{{Name}}", p.Name,
		"{{Event}}", p.Event,
		"{{Status}}", fmt.Sprintf("%d", p.Status),
		"{{VirtualPath}}", p.VirtualPath,
		"{{FsPath}}", p.FsPath,
		"{{VirtualTargetPath}}", p.VirtualTargetPath,
		"{{FsTargetPath}}", p.FsTargetPath,
		"{{ObjectName}}", p.ObjectName,
		"{{ObjectType}}", p.ObjectType,
		"{{FileSize}}", fmt.Sprintf("%d", p.FileSize),
		"{{Protocol}}", p.Protocol,
		"{{IP}}", p.IP,
		"{{Timestamp}}", fmt.Sprintf("%d", p.Timestamp),
	}
	if addObjectData {
		data, err := p.Object.RenderAsJSON(p.Event != operationDelete)
		if err == nil {
			replacements = append(replacements, "{{ObjectData}}", string(data))
		}
	}
	return replacements
}

func replaceWithReplacer(input string, replacer *strings.Replacer) string {
	if !strings.Contains(input, "{{") {
		return input
	}
	return replacer.Replace(input)
}

func checkEventConditionPattern(p dataprovider.ConditionPattern, name string) bool {
	matched, err := path.Match(p.Pattern, name)
	if err != nil {
		eventManagerLog(logger.LevelError, "pattern matching error %q, err: %v", p.Pattern, err)
		return false
	}
	if p.InverseMatch {
		return !matched
	}
	return matched
}

// checkConditionPatterns returns false if patterns are defined and no match is found
func checkEventConditionPatterns(name string, patterns []dataprovider.ConditionPattern) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if checkEventConditionPattern(p, name) {
			return true
		}
	}

	return false
}

func getHTTPRuleActionEndpoint(c dataprovider.EventActionHTTPConfig, replacer *strings.Replacer) (string, error) {
	if len(c.QueryParameters) > 0 {
		u, err := url.Parse(c.Endpoint)
		if err != nil {
			return "", fmt.Errorf("invalid endpoint: %w", err)
		}
		q := u.Query()

		for _, keyVal := range c.QueryParameters {
			q.Add(keyVal.Key, replaceWithReplacer(keyVal.Value, replacer))
		}

		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	return c.Endpoint, nil
}

func executeHTTPRuleAction(c dataprovider.EventActionHTTPConfig, params EventParams) error {
	if !c.Password.IsEmpty() {
		if err := c.Password.TryDecrypt(); err != nil {
			return fmt.Errorf("unable to decrypt password: %w", err)
		}
	}
	addObjectData := false
	if params.Object != nil {
		if !addObjectData {
			if strings.Contains(c.Body, "{{ObjectData}}") {
				addObjectData = true
			}
		}
	}

	replacements := params.getStringReplacements(addObjectData)
	replacer := strings.NewReplacer(replacements...)
	endpoint, err := getHTTPRuleActionEndpoint(c, replacer)
	if err != nil {
		return err
	}

	var body io.Reader
	if c.Body != "" && c.Method != http.MethodGet {
		body = bytes.NewBufferString(replaceWithReplacer(c.Body, replacer))
	}
	req, err := http.NewRequest(c.Method, endpoint, body)
	if err != nil {
		return err
	}
	if c.Username != "" {
		req.SetBasicAuth(replaceWithReplacer(c.Username, replacer), c.Password.GetAdditionalData())
	}
	for _, keyVal := range c.Headers {
		req.Header.Set(keyVal.Key, replaceWithReplacer(keyVal.Value, replacer))
	}
	client := c.GetHTTPClient()
	defer client.CloseIdleConnections()

	startTime := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		eventManagerLog(logger.LevelDebug, "unable to send http notification, endpoint: %s, elapsed: %s, err: %v",
			endpoint, time.Since(startTime), err)
		return err
	}
	defer resp.Body.Close()

	eventManagerLog(logger.LevelDebug, "http notification sent, endopoint: %s, elapsed: %s, status code: %d",
		endpoint, time.Since(startTime), resp.StatusCode)
	if resp.StatusCode < http.StatusOK || resp.StatusCode > http.StatusNoContent {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

func executeCommandRuleAction(c dataprovider.EventActionCommandConfig, params EventParams) error {
	envVars := make([]string, 0, len(c.EnvVars))
	addObjectData := false
	if params.Object != nil {
		for _, k := range c.EnvVars {
			if strings.Contains(k.Value, "{{ObjectData}}") {
				addObjectData = true
				break
			}
		}
	}
	replacements := params.getStringReplacements(addObjectData)
	replacer := strings.NewReplacer(replacements...)
	for _, keyVal := range c.EnvVars {
		envVars = append(envVars, fmt.Sprintf("%s=%s", keyVal.Key, replaceWithReplacer(keyVal.Value, replacer)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(c.Timeout)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Cmd)
	cmd.Env = append(cmd.Env, os.Environ()...)
	cmd.Env = append(cmd.Env, envVars...)

	startTime := time.Now()
	err := cmd.Run()

	eventManagerLog(logger.LevelDebug, "executed command %q, elapsed: %s, error: %v",
		c.Cmd, time.Since(startTime), err)

	return err
}

func executeEmailRuleAction(c dataprovider.EventActionEmailConfig, params EventParams) error {
	addObjectData := false
	if params.Object != nil {
		if strings.Contains(c.Body, "{{ObjectData}}") {
			addObjectData = true
		}
	}
	replacements := params.getStringReplacements(addObjectData)
	replacer := strings.NewReplacer(replacements...)
	body := replaceWithReplacer(c.Body, replacer)
	subject := replaceWithReplacer(c.Subject, replacer)
	startTime := time.Now()
	err := smtp.SendEmail(c.Recipients, subject, body, smtp.EmailContentTypeTextPlain)
	eventManagerLog(logger.LevelDebug, "executed email notification action, elapsed: %s, error: %v",
		time.Since(startTime), err)
	return err
}

func executeUsersQuotaResetRuleAction(conditions dataprovider.ConditionOptions) error {
	users, err := dataprovider.DumpUsers()
	if err != nil {
		return fmt.Errorf("unable to get users: %w", err)
	}
	var failedResets []string
	for _, user := range users {
		if !checkEventConditionPatterns(user.Username, conditions.Names) {
			eventManagerLog(logger.LevelDebug, "skipping scheduled quota reset for user %s, name conditions don't match",
				user.Username)
			continue
		}
		if !QuotaScans.AddUserQuotaScan(user.Username) {
			eventManagerLog(logger.LevelError, "another quota scan is already in progress for user %s", user.Username)
			failedResets = append(failedResets, user.Username)
			continue
		}
		numFiles, size, err := user.ScanQuota()
		QuotaScans.RemoveUserQuotaScan(user.Username)
		if err != nil {
			eventManagerLog(logger.LevelError, "error scanning quota for user %s: %v", user.Username, err)
			failedResets = append(failedResets, user.Username)
			continue
		}
		err = dataprovider.UpdateUserQuota(&user, numFiles, size, true)
		if err != nil {
			eventManagerLog(logger.LevelError, "error updating quota for user %s: %v", user.Username, err)
			failedResets = append(failedResets, user.Username)
			continue
		}
	}
	if len(failedResets) > 0 {
		return fmt.Errorf("quota reset failed for users: %+v", failedResets)
	}
	return nil
}

func executeFoldersQuotaResetRuleAction(conditions dataprovider.ConditionOptions) error {
	folders, err := dataprovider.DumpFolders()
	if err != nil {
		return fmt.Errorf("unable to get folders: %w", err)
	}
	var failedResets []string
	for _, folder := range folders {
		if !checkEventConditionPatterns(folder.Name, conditions.Names) {
			eventManagerLog(logger.LevelDebug, "skipping scheduled quota reset for folder %s, name conditions don't match",
				folder.Name)
			continue
		}
		if !QuotaScans.AddVFolderQuotaScan(folder.Name) {
			eventManagerLog(logger.LevelError, "another quota scan is already in progress for folder %s", folder.Name)
			failedResets = append(failedResets, folder.Name)
			continue
		}
		f := vfs.VirtualFolder{
			BaseVirtualFolder: folder,
			VirtualPath:       "/",
		}
		numFiles, size, err := f.ScanQuota()
		QuotaScans.RemoveVFolderQuotaScan(folder.Name)
		if err != nil {
			eventManagerLog(logger.LevelError, "error scanning quota for folder %s: %v", folder.Name, err)
			failedResets = append(failedResets, folder.Name)
			continue
		}
		err = dataprovider.UpdateVirtualFolderQuota(&folder, numFiles, size, true)
		if err != nil {
			eventManagerLog(logger.LevelError, "error updating quota for folder %s: %v", folder.Name, err)
			failedResets = append(failedResets, folder.Name)
			continue
		}
	}
	if len(failedResets) > 0 {
		return fmt.Errorf("quota reset failed for folders: %+v", failedResets)
	}
	return nil
}

func executeTransferQuotaResetRuleAction(conditions dataprovider.ConditionOptions) error {
	users, err := dataprovider.DumpUsers()
	if err != nil {
		return fmt.Errorf("unable to get users: %w", err)
	}
	var failedResets []string
	for _, user := range users {
		if !checkEventConditionPatterns(user.Username, conditions.Names) {
			eventManagerLog(logger.LevelDebug, "skipping scheduled transfer quota reset for user %s, name conditions don't match",
				user.Username)
			continue
		}
		err = dataprovider.UpdateUserTransferQuota(&user, 0, 0, true)
		if err != nil {
			eventManagerLog(logger.LevelError, "error updating transfer quota for user %s: %v", user.Username, err)
			failedResets = append(failedResets, user.Username)
			continue
		}
	}
	if len(failedResets) > 0 {
		return fmt.Errorf("transfer quota reset failed for users: %+v", failedResets)
	}
	return nil
}

func executeRuleAction(action dataprovider.BaseEventAction, params EventParams, conditions dataprovider.ConditionOptions) error {
	switch action.Type {
	case dataprovider.ActionTypeHTTP:
		return executeHTTPRuleAction(action.Options.HTTPConfig, params)
	case dataprovider.ActionTypeCommand:
		return executeCommandRuleAction(action.Options.CmdConfig, params)
	case dataprovider.ActionTypeEmail:
		return executeEmailRuleAction(action.Options.EmailConfig, params)
	case dataprovider.ActionTypeBackup:
		return dataprovider.ExecuteBackup()
	case dataprovider.ActionTypeUserQuotaReset:
		return executeUsersQuotaResetRuleAction(conditions)
	case dataprovider.ActionTypeFolderQuotaReset:
		return executeFoldersQuotaResetRuleAction(conditions)
	case dataprovider.ActionTypeTransferQuotaReset:
		return executeTransferQuotaResetRuleAction(conditions)
	default:
		return fmt.Errorf("unsupported action type: %d", action.Type)
	}
}

func executeSyncRulesActions(rules []dataprovider.EventRule, params EventParams) error {
	var errRes error

	for _, rule := range rules {
		var failedActions []string
		for _, action := range rule.Actions {
			if !action.Options.IsFailureAction && action.Options.ExecuteSync {
				startTime := time.Now()
				if err := executeRuleAction(action.BaseEventAction, params, rule.Conditions.Options); err != nil {
					eventManagerLog(logger.LevelError, "unable to execute sync action %q for rule %q, elapsed %s, err: %v",
						action.Name, rule.Name, time.Since(startTime), err)
					failedActions = append(failedActions, action.Name)
					// we return the last error, it is ok for now
					errRes = err
					if action.Options.StopOnFailure {
						break
					}
				} else {
					eventManagerLog(logger.LevelDebug, "executed sync action %q for rule %q, elapsed: %s",
						action.Name, rule.Name, time.Since(startTime))
				}
			}
		}
		// execute async actions if any, including failure actions
		go executeRuleAsyncActions(rule, params, failedActions)
	}

	return errRes
}

func executeAsyncRulesActions(rules []dataprovider.EventRule, params EventParams) {
	for _, rule := range rules {
		executeRuleAsyncActions(rule, params, nil)
	}
}

func executeRuleAsyncActions(rule dataprovider.EventRule, params EventParams, failedActions []string) {
	for _, action := range rule.Actions {
		if !action.Options.IsFailureAction && !action.Options.ExecuteSync {
			startTime := time.Now()
			if err := executeRuleAction(action.BaseEventAction, params, rule.Conditions.Options); err != nil {
				eventManagerLog(logger.LevelError, "unable to execute action %q for rule %q, elapsed %s, err: %v",
					action.Name, rule.Name, time.Since(startTime), err)
				failedActions = append(failedActions, action.Name)
				if action.Options.StopOnFailure {
					break
				}
			} else {
				eventManagerLog(logger.LevelDebug, "executed action %q for rule %q, elapsed %s",
					action.Name, rule.Name, time.Since(startTime))
			}
		}
	}
	if len(failedActions) > 0 {
		// execute failure actions
		for _, action := range rule.Actions {
			if action.Options.IsFailureAction {
				startTime := time.Now()
				if err := executeRuleAction(action.BaseEventAction, params, rule.Conditions.Options); err != nil {
					eventManagerLog(logger.LevelError, "unable to execute failure action %q for rule %q, elapsed %s, err: %v",
						action.Name, rule.Name, time.Since(startTime), err)
					if action.Options.StopOnFailure {
						break
					}
				} else {
					eventManagerLog(logger.LevelDebug, "executed failure action %q for rule %q, elapsed: %s",
						action.Name, rule.Name, time.Since(startTime))
				}
			}
		}
	}
}

type eventCronJob struct {
	ruleName string
}

func (j *eventCronJob) getTask(rule dataprovider.EventRule) (dataprovider.Task, error) {
	if rule.GuardFromConcurrentExecution() {
		task, err := dataprovider.GetTaskByName(rule.Name)
		if _, ok := err.(*util.RecordNotFoundError); ok {
			eventManagerLog(logger.LevelDebug, "adding task for rule %q", rule.Name)
			task = dataprovider.Task{
				Name:     rule.Name,
				UpdateAt: 0,
				Version:  0,
			}
			err = dataprovider.AddTask(rule.Name)
			if err != nil {
				eventManagerLog(logger.LevelWarn, "unable to add task for rule %q: %v", rule.Name, err)
				return task, err
			}
		} else {
			eventManagerLog(logger.LevelWarn, "unable to get task for rule %q: %v", rule.Name, err)
		}
		return task, err
	}

	return dataprovider.Task{}, nil
}

func (j *eventCronJob) Run() {
	eventManagerLog(logger.LevelDebug, "executing scheduled rule %q", j.ruleName)
	rule, err := dataprovider.EventRuleExists(j.ruleName)
	if err != nil {
		eventManagerLog(logger.LevelError, "unable to load rule with name %q", j.ruleName)
		return
	}
	task, err := j.getTask(rule)
	if err != nil {
		return
	}
	if task.Name != "" {
		updateInterval := 5 * time.Minute
		updatedAt := util.GetTimeFromMsecSinceEpoch(task.UpdateAt)
		if updatedAt.Add(updateInterval*2 + 1).After(time.Now()) {
			eventManagerLog(logger.LevelDebug, "task for rule %q too recent: %s, skip execution", rule.Name, updatedAt)
			return
		}
		err = dataprovider.UpdateTask(rule.Name, task.Version)
		if err != nil {
			eventManagerLog(logger.LevelInfo, "unable to update task timestamp for rule %q, skip execution, err: %v",
				rule.Name, err)
			return
		}
		ticker := time.NewTicker(updateInterval)
		done := make(chan bool)

		go func(taskName string) {
			eventManagerLog(logger.LevelDebug, "update task %q timestamp worker started", taskName)
			for {
				select {
				case <-done:
					eventManagerLog(logger.LevelDebug, "update task %q timestamp worker finished", taskName)
					return
				case <-ticker.C:
					err := dataprovider.UpdateTaskTimestamp(taskName)
					eventManagerLog(logger.LevelInfo, "updated timestamp for task %q, err: %v", taskName, err)
				}
			}
		}(task.Name)

		executeRuleAsyncActions(rule, EventParams{}, nil)

		done <- true
		ticker.Stop()
	} else {
		executeRuleAsyncActions(rule, EventParams{}, nil)
	}
	eventManagerLog(logger.LevelDebug, "execution for scheduled rule %q finished", j.ruleName)
}

func eventManagerLog(level logger.LogLevel, format string, v ...any) {
	logger.Log(level, "eventmanager", "", format, v...)
}