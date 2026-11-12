package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xleelz/fae/engine"
)

// TaskNotification aliases engine.TaskNotification for provider-layer brief formatting (FAE-OPS-033).
type TaskNotification = engine.TaskNotification

// TurnBudgetSpec aliases engine.TurnBudgetSpec for provider-layer follow-up turns (FAE-OPS-034).
type TurnBudgetSpec = engine.TurnBudgetSpec

var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
)

// EscapeQuotedText escapes untrusted notification text so it can never break
// out of a quoted-data element (FAE-WF-043).
func EscapeQuotedText(s string) string {
	return xmlEscaper.Replace(s)
}

// FormatNotificationsQuoted renders task notifications inside a quoted-data
// <notifications> container with <notification> elements carrying kind,
// from_task, delivery, and relayed_by provenance (FAE-OPS-033, FAE-WF-043).
func FormatNotificationsQuoted(notifications []engine.TaskNotification) string {
	if len(notifications) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<notifications data_only="true">`)
	for _, n := range notifications {
		b.WriteString("\n  <notification")
		if n.ID != "" {
			fmt.Fprintf(&b, ` id="%s"`, EscapeQuotedText(n.ID))
		}
		fmt.Fprintf(&b, ` task="%s"`, EscapeQuotedText(n.TaskID))
		fmt.Fprintf(&b, ` kind="%s"`, EscapeQuotedText(n.Kind))
		if n.FromTaskID != "" {
			fmt.Fprintf(&b, ` from_task="%s"`, EscapeQuotedText(n.FromTaskID))
		}
		delivery := n.Delivery
		if delivery == "" {
			delivery = "next-turn"
		}
		fmt.Fprintf(&b, ` delivery="%s"`, EscapeQuotedText(delivery))
		if n.RelayedBy != "" {
			fmt.Fprintf(&b, ` relayed_by="%s"`, EscapeQuotedText(n.RelayedBy))
		}
		b.WriteString(">")
		b.WriteString(EscapeQuotedText(n.Body))
		b.WriteString("</notification>")
	}
	b.WriteString("\n</notifications>")
	return b.String()
}

// BuildNotificationsBriefBlock constructs the brief block for task notifications
// bound as quoted data (FAE-OPS-033, FAE-WF-043), or nil when none are present.
func BuildNotificationsBriefBlock(notifications []engine.TaskNotification) any {
	if len(notifications) == 0 {
		return nil
	}
	items := make([]map[string]any, 0, len(notifications))
	for _, n := range notifications {
		delivery := n.Delivery
		if delivery == "" {
			delivery = "next-turn"
		}
		item := map[string]any{
			"id":        n.ID,
			"task_id":   n.TaskID,
			"kind":      n.Kind,
			"delivery":  delivery,
			"body":      n.Body,
			"quoted":    FormatNotificationsQuoted([]engine.TaskNotification{n}),
			"data_only": true,
		}
		if n.FromTaskID != "" {
			item["from_task"] = n.FromTaskID
			item["from_task_id"] = n.FromTaskID
		}
		if n.RelayedBy != "" {
			item["relayed_by"] = n.RelayedBy
		}
		if !n.CreatedAt.IsZero() {
			item["created_at"] = n.CreatedAt.UTC().Format(time.RFC3339Nano)
		}
		items = append(items, item)
	}
	return map[string]any{
		"kind":      "notifications",
		"data_only": true,
		"quoted":    FormatNotificationsQuoted(notifications),
		"items":     items,
	}
}

// LiveNotificationPath returns the path of the live in-flight notification file
// for taskID under root.
func LiveNotificationPath(root, taskID string) string {
	sum := strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(taskID)
	return filepath.Join(root, "notifications", sum+".json")
}

// WriteLiveNotification appends an in-flight notification to the live
// notification file for taskID so tool-boundary hooks can deliver it mid-turn.
func WriteLiveNotification(root, taskID string, n engine.TaskNotification) (string, error) {
	if root == "" || taskID == "" {
		return "", nil
	}
	path := LiveNotificationPath(root, taskID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	var existing []engine.TaskNotification
	if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &existing)
	}
	replaced := false
	for i := range existing {
		if existing[i].ID == n.ID && n.ID != "" {
			existing[i] = n
			replaced = true
			break
		}
	}
	if !replaced {
		existing = append(existing, n)
	}
	raw, err := json.Marshal(existing)
	if err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// ConsumeLiveNotifications reads undelivered in-flight notifications from path,
// marks them delivered at now, persists the updated file, and returns the
// newly delivered notifications along with their quoted-data string.
func ConsumeLiveNotifications(path string, now time.Time) ([]engine.TaskNotification, string, error) {
	if path == "" {
		return nil, "", nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	var list []engine.TaskNotification
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, "", err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var pending []engine.TaskNotification
	for i := range list {
		if list[i].DeliveredAt.IsZero() {
			list[i].DeliveredAt = now.UTC()
			pending = append(pending, list[i])
		}
	}
	if len(pending) == 0 {
		return nil, "", nil
	}
	updated, err := json.Marshal(list)
	if err != nil {
		return nil, "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, updated, 0600); err == nil {
		_ = os.Rename(tmp, path)
	}
	return pending, FormatNotificationsQuoted(pending), nil
}
