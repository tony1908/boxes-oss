package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/tony1908/boxes-oss/internal/types"
)

// Users and groups are Linux accounts inside the box. Every step runs as root through the agent exec
// endpoint with argv (no shell), so names never reach a shell. Accounts live in the box filesystem and
// persist across stop/resume like other system changes.

var accountNamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// validateAccountName checks a user or group name. root and abc are the box's built-in accounts.
func validateAccountName(kind, name string) error {
	if !accountNamePattern.MatchString(name) {
		return fmt.Errorf("%s name must match ^[a-z_][a-z0-9_-]{0,31}$", kind)
	}
	if name == "root" || name == "abc" {
		return fmt.Errorf("%s name %q is reserved", kind, name)
	}
	return nil
}

// createUserCommands is the root command sequence for a new user. abc joins the user's group so it can
// read the home (homes are 770). abc's already-running processes gain the group only after a restart.
func createUserCommands(name string) [][]string {
	home := "/home/" + name
	return [][]string{
		{"useradd", "-m", "-s", "/bin/bash", name},
		{"chmod", "770", home},
		{"usermod", "-aG", name, "abc"},
	}
}

// deleteUserCommands removes the user and its home directory.
func deleteUserCommands(name string) [][]string {
	return [][]string{{"userdel", "-r", name}}
}

// createGroupCommands creates a group and its setgid shared directory; abc joins the group.
func createGroupCommands(name string) [][]string {
	shared := "/shared/" + name
	return [][]string{
		{"groupadd", name},
		{"mkdir", "-p", shared},
		{"chgrp", name, shared},
		{"chmod", "2770", shared},
		{"usermod", "-aG", name, "abc"},
	}
}

// addMemberCommands adds a user to a group.
func addMemberCommands(group, user string) [][]string {
	return [][]string{{"usermod", "-aG", group, user}}
}

// removeMemberCommands removes a user from a group. Removal affects only the user's new processes.
func removeMemberCommands(group, user string) [][]string {
	return [][]string{{"gpasswd", "-d", user, group}}
}

// listUsersCommands reads the passwd and group databases; two execs cover all users.
func listUsersCommands() [][]string {
	return [][]string{{"getent", "passwd"}, {"getent", "group"}}
}

// boxUser is one account in a box's home area.
type boxUser struct {
	Username string   `json:"username"`
	HomeDir  string   `json:"homeDir"`
	Groups   []string `json:"groups"`
}

// parseUsers builds the /home/* users from getent passwd and getent group output.
func parseUsers(passwd, group string) []boxUser {
	groupNames := map[string]string{}
	members := map[string][]string{}
	scanner := bufio.NewScanner(strings.NewReader(group))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 4 {
			continue
		}
		groupNames[fields[2]] = fields[0]
		if fields[3] != "" {
			for _, member := range strings.Split(fields[3], ",") {
				members[member] = append(members[member], fields[0])
			}
		}
	}
	users := []boxUser{}
	scanner = bufio.NewScanner(strings.NewReader(passwd))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 7 || !strings.HasPrefix(fields[5], "/home/") {
			continue
		}
		// System accounts such as dockremap are not box users: keep uid >= 1001 and skip nobody (65534).
		if uid, err := strconv.Atoi(fields[2]); err != nil || uid < 1001 || uid == 65534 {
			continue
		}
		groups := []string{}
		if primary, ok := groupNames[fields[3]]; ok {
			groups = append(groups, primary)
		}
		groups = append(groups, members[fields[0]]...)
		users = append(users, boxUser{Username: fields[0], HomeDir: fields[5], Groups: uniqueStrings(groups)})
	}
	return users
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// boxStepError is a root step that exited non-zero: its exit code and stderr.
type boxStepError struct {
	code   int
	stderr string
}

func (e *boxStepError) Error() string {
	return fmt.Sprintf("exit %d: %s", e.code, e.stderr)
}

// runBoxRootSteps runs each argv as root in the box, stopping at the first non-zero exit.
// It returns each step's stdout, or a boxStepError for a failing step.
func (s *Server) runBoxRootSteps(ctx context.Context, boxID string, steps [][]string) ([]string, error) {
	_, client, err := s.loadBoxAgent(ctx, boxID)
	if err != nil {
		return nil, err
	}
	outputs := make([]string, 0, len(steps))
	for _, step := range steps {
		var result struct {
			ExitCode int    `json:"exitCode"`
			Stdout   string `json:"stdout"`
			Stderr   string `json:"stderr"`
		}
		body := map[string]any{"cmd": step, "user": "root"}
		if err := client.json(ctx, http.MethodPost, agentPath("v1", "boxes", boxID, "exec"), body, &result); err != nil {
			return nil, err
		}
		if result.ExitCode != 0 {
			return nil, &boxStepError{code: result.ExitCode, stderr: strings.TrimSpace(result.Stderr)}
		}
		outputs = append(outputs, result.Stdout)
	}
	return outputs, nil
}

// accountError maps a failed root step to an HTTP response. Exit codes come from shadow-utils:
// 9 means the name already exists and 6 means the user or group does not exist.
func (s *Server) accountError(w http.ResponseWriter, err error, existsCode, existsError, failCode, failMessage string) {
	var step *boxStepError
	if errors.As(err, &step) {
		switch step.code {
		case 9:
			writeError(w, http.StatusConflict, existsCode, existsError)
		case 6:
			writeError(w, http.StatusNotFound, "not_found", "user or group does not exist")
		default:
			writeError(w, http.StatusBadGateway, failCode, failMessage)
		}
		return
	}
	s.apiError(w, err)
}

// checkRunningBox returns false (after writing 409) unless the box is ready or idle.
func (s *Server) checkRunningBox(w http.ResponseWriter, r *http.Request, boxID string) bool {
	record, err := s.store.GetBox(r.Context(), boxID)
	if err != nil {
		s.apiError(w, err)
		return false
	}
	if record.State != types.StateReady && record.State != types.StateIdle {
		writeError(w, http.StatusConflict, "sandbox_not_running", "sandbox must be running to manage users and groups")
		return false
	}
	return true
}

// accountAction records a user or group change in the activity log.
func (s *Server) accountAction(ctx context.Context, boxID, action string, detail map[string]any, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	s.recordActivity(ctx, boxID, "access", action, detail, outcome)
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	boxID := pathID(r)
	var input struct {
		Username string `json:"username"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid user request")
		return
	}
	if err := validateAccountName("user", input.Username); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_user", err.Error())
		return
	}
	if !s.checkRunningBox(w, r, boxID) {
		return
	}
	_, err := s.runBoxRootSteps(r.Context(), boxID, createUserCommands(input.Username))
	s.accountAction(r.Context(), boxID, "user created", map[string]any{"username": input.Username}, err)
	if err != nil {
		s.accountError(w, err, "user_exists", "user already exists", "user_create_failed", "user could not be created")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"username": input.Username, "homeDir": "/home/" + input.Username})
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	boxID := pathID(r)
	if !s.checkRunningBox(w, r, boxID) {
		return
	}
	outputs, err := s.runBoxRootSteps(r.Context(), boxID, listUsersCommands())
	if err != nil {
		s.accountError(w, err, "user_exists", "user already exists", "user_list_failed", "users could not be listed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": parseUsers(outputs[0], outputs[1])})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	boxID, name := pathID(r), r.PathValue("username")
	if err := validateAccountName("user", name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_user", err.Error())
		return
	}
	if !s.checkRunningBox(w, r, boxID) {
		return
	}
	_, err := s.runBoxRootSteps(r.Context(), boxID, deleteUserCommands(name))
	s.accountAction(r.Context(), boxID, "user deleted", map[string]any{"username": name}, err)
	if err != nil {
		var step *boxStepError
		if errors.As(err, &step) && step.code == 6 {
			writeError(w, http.StatusNotFound, "user_not_found", "user does not exist")
			return
		}
		s.accountError(w, err, "user_exists", "user already exists", "user_delete_failed", "user could not be deleted")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	boxID := pathID(r)
	var input struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid group request")
		return
	}
	if err := validateAccountName("group", input.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_group", err.Error())
		return
	}
	if !s.checkRunningBox(w, r, boxID) {
		return
	}
	_, err := s.runBoxRootSteps(r.Context(), boxID, createGroupCommands(input.Name))
	s.accountAction(r.Context(), boxID, "group created", map[string]any{"group": input.Name}, err)
	if err != nil {
		s.accountError(w, err, "group_exists", "group already exists", "group_create_failed", "group could not be created")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": input.Name, "sharedDir": "/shared/" + input.Name})
}

func (s *Server) addGroupMember(w http.ResponseWriter, r *http.Request) {
	boxID, group, user := pathID(r), r.PathValue("name"), r.PathValue("username")
	if err := errors.Join(validateAccountName("group", group), validateAccountName("user", user)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_membership", err.Error())
		return
	}
	if !s.checkRunningBox(w, r, boxID) {
		return
	}
	_, err := s.runBoxRootSteps(r.Context(), boxID, addMemberCommands(group, user))
	s.accountAction(r.Context(), boxID, "group member added", map[string]any{"group": group, "username": user}, err)
	if err != nil {
		s.accountError(w, err, "user_exists", "user already exists", "membership_failed", "user could not be added to the group")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"group": group, "username": user})
}

// removeGroupMember takes effect for the user's new processes only; running processes keep the group.
func (s *Server) removeGroupMember(w http.ResponseWriter, r *http.Request) {
	boxID, group, user := pathID(r), r.PathValue("name"), r.PathValue("username")
	if err := errors.Join(validateAccountName("group", group), validateAccountName("user", user)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_membership", err.Error())
		return
	}
	if !s.checkRunningBox(w, r, boxID) {
		return
	}
	_, err := s.runBoxRootSteps(r.Context(), boxID, removeMemberCommands(group, user))
	s.accountAction(r.Context(), boxID, "group member removed", map[string]any{"group": group, "username": user}, err)
	if err != nil {
		s.accountError(w, err, "user_exists", "user already exists", "membership_failed", "user could not be removed from the group")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
