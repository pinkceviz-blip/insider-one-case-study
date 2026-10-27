package studio

import "testing"

func TestCampaignRateWaitCountsAsQueued(t *testing.T) {
	ctx, e, _ := auditFixture(t)
	cid := auditCampaign(t, ctx, e, "İstanbul")
	for _, ch := range []string{"sms", "email"} {
		for _, code := range []string{"", "rate_wait", "throttled:429", "transient:503"} {
			id, _, _ := auditDelivery(t, ctx, e, cid, ch)
			if code != "" {
				if _, err := e.Store.DB.Exec(ctx, "UPDATE deliveries SET state='retrying',error=$2 WHERE id=$1", id, code); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	c, err := e.Store.Campaign(ctx, cid)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"sms", "email"} {
		if c.ChannelCounts[ch]["pending"] != 2 || c.ChannelCounts[ch]["retrying"] != 2 {
			t.Fatalf("%s: expected 2 queued and 2 provider retries, got %v", ch, c.ChannelCounts[ch])
		}
	}
	if c.Counts["pending"] != 2 || c.Counts["retrying"] != 6 {
		t.Fatalf("durable states changed: %v", c.Counts)
	}
	if c.Status != "dispatching" {
		t.Fatalf("waiting deliveries must keep campaign active: %s", c.Status)
	}
}
