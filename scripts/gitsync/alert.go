package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// alerter tells a person about what the syncer will not resolve on its own:
// a diverged branch, a tag that differs, or a side it cannot reach. Each
// problem is reported when it appears and again every repeat while it lasts;
// one that clears is forgotten, so it is reported afresh if it comes back.
type alerter struct {
	webhook string // DingTalk robot URL; empty logs only
	repeat  time.Duration
	client  *http.Client
	now     func() time.Time
	logf    func(format string, args ...any)
	sent    map[string]time.Time
}

// report takes the full set of current problems, keyed by what identifies
// one (a diverged ref with both commits, say), and sends those that are due.
func (a *alerter) report(ctx context.Context, problems map[string]string) {
	if a.sent == nil {
		a.sent = map[string]time.Time{}
	}
	for key := range a.sent {
		if _, open := problems[key]; !open {
			delete(a.sent, key)
		}
	}
	keys := make([]string, 0, len(problems))
	for key := range problems {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var due []string
	for _, key := range keys {
		if last, ok := a.sent[key]; ok && a.now().Sub(last) < a.repeat {
			continue
		}
		due = append(due, problems[key])
		a.sent[key] = a.now()
	}
	if len(due) == 0 {
		return
	}
	text := "gpuworks gitsync needs a person:\n- " + strings.Join(due, "\n- ")
	a.logf("alert: %s", text)
	if a.webhook == "" {
		return
	}
	if err := a.send(ctx, text); err != nil {
		a.logf("alert not delivered: %v", err)
		// Retry on the next cycle rather than after a full repeat.
		for _, key := range keys {
			delete(a.sent, key)
		}
	}
}

func (a *alerter) send(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": text}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var answer struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return fmt.Errorf("dingtalk answered %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || answer.ErrCode != 0 {
		return fmt.Errorf("dingtalk answered %d errcode %d %s", resp.StatusCode, answer.ErrCode, answer.ErrMsg)
	}
	return nil
}
