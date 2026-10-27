package studio

import (
	"errors"
	"github.com/hibiken/asynq"
	"testing"
)

type stubQueueInspector struct {
	q   *asynq.QueueInfo
	err error
}

func (s stubQueueInspector) GetQueueInfo(string) (*asynq.QueueInfo, error) { return s.q, s.err }
func TestQueueTotalIncludesDelayedAndRetryJobs(t *testing.T) {
	q := readQueueMetrics(stubQueueInspector{q: &asynq.QueueInfo{Pending: 0, Active: 0, Scheduled: 7, Retry: 95, Archived: 4, Completed: 800}}, []string{"sms"})["sms"]
	if !q.Available || q.Outstanding != 102 || q.Retry != 95 || q.Pending != 0 {
		t.Fatalf("retry jobs hidden: %+v", q)
	}
}
func TestQueueUnavailableIsNotAnEmptyQueue(t *testing.T) {
	broken := readQueueMetrics(stubQueueInspector{err: errors.New("redis down")}, []string{"sms"})["sms"]
	empty := readQueueMetrics(stubQueueInspector{err: asynq.ErrQueueNotFound}, []string{"sms"})["sms"]
	if broken.Available || broken.Error == "" || !empty.Available || empty.Outstanding != 0 {
		t.Fatalf("bad states broken=%+v empty=%+v", broken, empty)
	}
}

func (s stubQueueInspector) Queues() ([]string, error) {
	if errors.Is(s.err, asynq.ErrQueueNotFound) {
		return []string{}, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return []string{"sms"}, nil
}
