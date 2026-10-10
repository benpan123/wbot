package wbot

import (
	"strings"
	"time"
)

// Counters are process-local transport observations, not model/token accounting.
type Activity struct {
	StartedAt        time.Time `json:"started_at"`
	LastReceiveAt    time.Time `json:"last_receive_at"`
	LastDeliveryAt   time.Time `json:"last_delivery_at"`
	LastProgressAt   time.Time `json:"last_progress_at"`
	EmptyPolls       uint64    `json:"empty_polls"`
	ReceivedEvents   uint64    `json:"received_events"`
	DeliveryAttempts uint64    `json:"delivery_attempts"`
	DeliveredEvents  uint64    `json:"delivered_events"`
}

type HealthResult struct {
	Healthy  bool     `json:"healthy"`
	Error    string   `json:"error,omitempty"`
	Issues   []string `json:"issues"`
	Activity Activity `json:"activity"`
}

func (r *Relay) progress() {
	r.statusLock.Lock()
	r.activity.LastProgressAt = time.Now().UTC()
	r.statusLock.Unlock()
}

// Health performs no network request, hook invocation, or disk write.
func (r *Relay) Health() HealthResult {
	loggedIn, _, _, _ := r.Journal.Summary()
	r.statusLock.RLock()
	activity, issue := r.activity, r.lastIssue
	r.statusLock.RUnlock()
	issues := []string{}
	if !loggedIn {
		issues = append(issues, "尚未保存微信登录凭证")
	}
	if activity.LastReceiveAt.IsZero() {
		issues = append(issues, "尚未完成首次微信消息检查")
	}
	if !activity.LastProgressAt.IsZero() && time.Since(activity.LastProgressAt) > 5*time.Minute {
		issues = append(issues, "消息处理流程超过五分钟未更新")
	}
	if issue != "" {
		issues = append(issues, issue)
	}
	return HealthResult{Healthy: len(issues) == 0, Issues: issues, Error: strings.Join(issues, "；"), Activity: activity}
}
