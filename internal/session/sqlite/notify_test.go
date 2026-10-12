package sqlite

import (
	"context"
	"testing"
)

func TestNotificationLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()

	id1, err := s.PushNotification(ctx, "t1", "cron", "每天九点的提醒", "去晨会")
	if err != nil {
		t.Fatalf("PushNotification: %v", err)
	}
	if _, err := s.PushNotification(ctx, "t1", "cron", "周报", "写周报"); err != nil {
		t.Fatalf("PushNotification: %v", err)
	}
	if _, err := s.PushNotification(ctx, "t2", "cron", "别人的任务", "与 t1 无关"); err != nil {
		t.Fatalf("PushNotification: %v", err)
	}

	rows, err := s.ListNotifications(ctx, "t1")
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Title != "周报" || rows[0].Body != "写周报" || rows[0].Kind != "cron" {
		t.Fatalf("newest first violated: %+v", rows[0])
	}
	if rows[0].ID == id1 {
		t.Fatalf("ids must differ: %d", id1)
	}
	if rows[0].CreatedAt <= 0 {
		t.Fatalf("created_at missing: %+v", rows[0])
	}

	n, err := s.AckNotifications(ctx, "t1", []int64{rows[0].ID, 9999})
	if err != nil || n != 1 {
		t.Fatalf("ack = %d, %v; want 1 (unknown id ignored)", n, err)
	}
	// 幂等：重复 ack 生效 0。
	if n, err := s.AckNotifications(ctx, "t1", []int64{rows[0].ID}); err != nil || n != 0 {
		t.Fatalf("re-ack = %d, %v; want 0", n, err)
	}
	// 越租户 ack 不生效。
	if n, err := s.AckNotifications(ctx, "t2", []int64{id1}); err != nil || n != 0 {
		t.Fatalf("cross-tenant ack = %d, %v; want 0", n, err)
	}
	// 空入参零行为。
	if n, err := s.AckNotifications(ctx, "t1", nil); err != nil || n != 0 {
		t.Fatalf("empty ack = %d, %v", n, err)
	}

	rows, err = s.ListNotifications(ctx, "t1")
	if err != nil || len(rows) != 1 || rows[0].ID != id1 {
		t.Fatalf("after ack = %+v, %v; want only id1", rows, err)
	}
}
