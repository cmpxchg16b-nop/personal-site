package ss

import (
	"context"
	"errors"
	"testing"
)

func TestOnMemoryGroupRepository(t *testing.T) {
	ctx := context.Background()
	r := NewOnMemoryGroupRepository()

	// A miss is (nil, nil), not an error.
	g, err := r.GetGroupBySubscriberId(ctx, WellKnownChIdMain, "team")
	if err != nil || g != nil {
		t.Fatalf("miss = %v, %v, want (nil, nil)", g, err)
	}

	// Create + get.
	team := &Group{ChannelId: WellKnownChIdMain, SubscriberId: "team", Name: "team"}
	if err := r.CreateGroup(ctx, team); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	// Mutating the argument afterwards must not alias the stored group.
	team.Name = "mutated"
	g, err = r.GetGroupBySubscriberId(ctx, WellKnownChIdMain, "team")
	if err != nil || g == nil || g.Name != "team" {
		t.Fatalf("get = %+v, %v, want the team group", g, err)
	}

	// A duplicate (channel id, subscriber id) is rejected.
	if err := r.CreateGroup(ctx, &Group{ChannelId: WellKnownChIdMain, SubscriberId: "team", Name: "dupe"}); !errors.Is(err, ErrGroupExists) {
		t.Fatalf("duplicate create = %v, want ErrGroupExists", err)
	}

	// Listing is per channel and sorted by subscriber id.
	for _, g := range []*Group{
		{ChannelId: WellKnownChIdMain, SubscriberId: "1005", Name: "b"},
		{ChannelId: WellKnownChIdMain, SubscriberId: "1001", Name: "a"},
		{ChannelId: "another-channel", SubscriberId: "elsewhere", Name: "x"},
	} {
		if err := r.CreateGroup(ctx, g); err != nil {
			t.Fatalf("CreateGroup(%q): %v", g.SubscriberId, err)
		}
	}
	groups, err := r.GetGroupsByChannelId(ctx, WellKnownChIdMain)
	if err != nil {
		t.Fatalf("GetGroupsByChannelId: %v", err)
	}
	if len(groups) != 3 || groups[0].SubscriberId != "1001" || groups[1].SubscriberId != "1005" ||
		groups[2].SubscriberId != "team" {
		t.Fatalf("groups = %+v, want 1001, 1005, team (sorted, this channel only)", groups)
	}

	// Delete removes; deleting an absent group is a no-op.
	if err := r.DeleteGroup(ctx, WellKnownChIdMain, "team"); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	if err := r.DeleteGroup(ctx, WellKnownChIdMain, "team"); err != nil {
		t.Fatalf("DeleteGroup of an absent group: %v", err)
	}
	g, err = r.GetGroupBySubscriberId(ctx, WellKnownChIdMain, "team")
	if err != nil || g != nil {
		t.Fatalf("get after delete = %v, %v, want (nil, nil)", g, err)
	}

	// A cancelled context aborts every operation.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.CreateGroup(cancelled, &Group{ChannelId: WellKnownChIdMain, SubscriberId: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("create over a cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := r.GetGroupsByChannelId(cancelled, WellKnownChIdMain); !errors.Is(err, context.Canceled) {
		t.Fatalf("list over a cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := r.GetGroupBySubscriberId(cancelled, WellKnownChIdMain, "1001"); !errors.Is(err, context.Canceled) {
		t.Fatalf("get over a cancelled ctx = %v, want context.Canceled", err)
	}
	if err := r.DeleteGroup(cancelled, WellKnownChIdMain, "1001"); !errors.Is(err, context.Canceled) {
		t.Fatalf("delete over a cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestOnMemoryGroupRepositoryMembership(t *testing.T) {
	ctx := context.Background()
	r := NewOnMemoryGroupRepository()

	membership := func(groupId, member SubscriberId) (GroupMembership, bool) {
		t.Helper()
		m, ok := r.members[WellKnownChIdMain][groupId][member]
		return m, ok
	}

	// Joining a group that does not exist fails.
	if err := r.JoinGroup(ctx, WellKnownChIdMain, "team", "alice", "alice", GroupRoleMember); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("join unknown group = %v, want ErrGroupNotFound", err)
	}
	if err := r.CreateGroup(ctx, &Group{ChannelId: WellKnownChIdMain, SubscriberId: "team", Name: "team"}); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// x joins G as member, done by x themselves: operator == member.
	if err := r.JoinGroup(ctx, WellKnownChIdMain, "team", "alice", "alice", GroupRoleMember); err != nil {
		t.Fatalf("JoinGroup: %v", err)
	}
	m, ok := membership("team", "alice")
	if !ok || m.Role != GroupRoleMember || m.Operator != "alice" || m.GroupId != "team" {
		t.Fatalf("membership = %+v, %v, want alice a self-joined member of team", m, ok)
	}

	// A repetitive join with an identical tuple is a no-op.
	if err := r.JoinGroup(ctx, WellKnownChIdMain, "team", "alice", "alice", GroupRoleMember); err != nil {
		t.Fatalf("repetitive JoinGroup: %v", err)
	}
	if m2, _ := membership("team", "alice"); m2 != m {
		t.Fatalf("membership after an identical re-join = %+v, want unchanged %+v", m2, m)
	}

	// An admin invites bob: operator != member records the operator.
	if err := r.JoinGroup(ctx, WellKnownChIdMain, "team", "alice", "bob", GroupRoleMember); err != nil {
		t.Fatalf("JoinGroup(bob): %v", err)
	}
	if m, _ := membership("team", "bob"); m.Operator != "alice" {
		t.Fatalf("bob's membership operator = %q, want alice (invited by her)", m.Operator)
	}

	// The lower the role number, the higher the privilege — so bob's
	// re-join as an admin (1) after being a member (2) is a promotion.
	if GroupRoleOwner >= GroupRoleAdmin || GroupRoleAdmin >= GroupRoleMember {
		t.Fatal("role numbers must run owner < admin < member (lower number = higher privilege)")
	}
	if err := r.JoinGroup(ctx, WellKnownChIdMain, "team", "alice", "bob", GroupRoleAdmin); err != nil {
		t.Fatalf("promotion JoinGroup: %v", err)
	}
	if m, _ := membership("team", "bob"); m.Role != GroupRoleAdmin || m.Operator != "alice" {
		t.Fatalf("membership after promotion = %+v, want bob an admin (by alice)", m)
	}

	// Leaving removes the membership; leaving again (or an unknown group)
	// is a no-op.
	if err := r.LeaveGroup(ctx, WellKnownChIdMain, "team", "alice", "bob"); err != nil {
		t.Fatalf("LeaveGroup: %v", err)
	}
	if _, ok := membership("team", "bob"); ok {
		t.Fatal("bob is still a member after alice kicked him")
	}
	if err := r.LeaveGroup(ctx, WellKnownChIdMain, "team", "bob", "bob"); err != nil {
		t.Fatalf("LeaveGroup of a non-member: %v", err)
	}
	if err := r.LeaveGroup(ctx, WellKnownChIdMain, "no-such-group", "bob", "bob"); err != nil {
		t.Fatalf("LeaveGroup of an unknown group: %v", err)
	}

	// Deleting the group drops its memberships.
	if err := r.DeleteGroup(ctx, WellKnownChIdMain, "team"); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	if _, ok := membership("team", "alice"); ok {
		t.Fatal("alice's membership survived the group's deletion")
	}

	// A cancelled context aborts both.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.JoinGroup(cancelled, WellKnownChIdMain, "team", "a", "a", GroupRoleMember); !errors.Is(err, context.Canceled) {
		t.Fatalf("join over a cancelled ctx = %v, want context.Canceled", err)
	}
	if err := r.LeaveGroup(cancelled, WellKnownChIdMain, "team", "a", "a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("leave over a cancelled ctx = %v, want context.Canceled", err)
	}
}
