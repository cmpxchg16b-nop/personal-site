package ss

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// Group is a group chat — the first instance of a virtual subscriber:
// a subscriber of a channel that exists WITHOUT any real UA registration
// behind it (see docs/group-chat.md). Groups are never registered — no
// client can register with subscriberType "group", and the register
// event carries no type at all. A group exists purely as a
// GroupRepository entry that the SS blends into the channel's member
// listing and resolves in profile queries.
type Group struct {
	// ChannelId is the channel the group lives in.
	ChannelId ChannelId

	// SubscriberId is the group's subscriber id within the channel. A
	// group's id is held: a person registering the same id is rejected
	// with ErrorCodeSubscriberIdTakenByGroup.
	SubscriberId SubscriberId

	// Name is the group's display name — what profile queries report as
	// the username.
	Name string
}

// ErrGroupExists is returned by GroupRepository.CreateGroup when a group
// with the same (channel id, subscriber id) pair already exists.
var ErrGroupExists = errors.New("group already exists")

// ErrGroupNotFound is returned when an operation names a group that does
// not exist.
var ErrGroupNotFound = errors.New("group not found")

// GroupRole is the role a member holds in a group. The lower the number,
// the higher the privilege: owner 0, admin 1, member 2 — so a join
// re-recording a membership with a LOWER-numbered role is a promotion.
type GroupRole int

const (
	// GroupRoleOwner is the owner of the group — the highest privilege.
	GroupRoleOwner GroupRole = iota

	// GroupRoleAdmin is an administrator of the group.
	GroupRoleAdmin

	// GroupRoleMember is an ordinary member of the group.
	GroupRoleMember
)

// GroupMembership records that a subscriber (Member) joined a group
// (GroupId, in ChannelId) as Role: the tuple (member, role, group).
type GroupMembership struct {
	ChannelId ChannelId

	// GroupId is the group's subscriber id within the channel.
	GroupId SubscriberId

	// Member is the subscriber id of the member, within the same channel.
	Member SubscriberId

	Role GroupRole

	// Operator is who performed the join — or the latest role change —
	// identified by its subscriber id in the same channel. When the
	// operator IS the member, the member did it themselves; otherwise it
	// records an action ON the member, e.g. an admin inviting or
	// promoting them.
	Operator SubscriberId
}

// GroupRepository stores the groups of channels — the virtual
// subscribers — and their memberships. It is a dependency of the SS
// provider, injected as an interface: the provider never relies on a
// concrete implementation.
type GroupRepository interface {
	// CreateGroup adds a group. It fails with ErrGroupExists when a
	// group with the same (channel id, subscriber id) pair already
	// exists.
	CreateGroup(ctx context.Context, group *Group) error

	// DeleteGroup removes the group with the given (channel id,
	// subscriber id) pair. Deleting a group that does not exist is a
	// no-op.
	DeleteGroup(ctx context.Context, channelId ChannelId, subscriberId SubscriberId) error

	// GetGroupsByChannelId returns the groups of a channel, sorted by
	// subscriber id.
	GetGroupsByChannelId(ctx context.Context, channelId ChannelId) ([]*Group, error)

	// GetGroupBySubscriberId returns the group with the given (channel
	// id, subscriber id) pair, or (nil, nil) when no such group exists.
	GetGroupBySubscriberId(ctx context.Context, channelId ChannelId, subscriberId SubscriberId) (*Group, error)

	// JoinGroup records the tuple (member, role, group), performed by
	// operator. Repetitive joins are allowed: re-recording an identical
	// tuple — same member, same role, same group — is a no-op; a
	// re-join with a different role updates the membership, a promotion
	// when the new role is lower-numbered (the lower the number, the
	// higher the privilege), a demotion when higher-numbered. When
	// operator == member the member did it themselves;
	// otherwise the call records an action on the member — e.g. an admin
	// inviting or promoting them. The repository RECORDS the operator;
	// authorization (who may add whom) is the caller's policy. It fails
	// with ErrGroupNotFound when the group does not exist.
	JoinGroup(ctx context.Context, channelId ChannelId, groupId SubscriberId, operator SubscriberId, member SubscriberId, role GroupRole) error

	// LeaveGroup removes the member's membership of the group, performed
	// by operator (operator == member means the member left on their
	// own; otherwise e.g. an admin kicked the member). Leaving a group
	// one is not a member of is a no-op, as is leaving a group that does
	// not exist.
	LeaveGroup(ctx context.Context, channelId ChannelId, groupId SubscriberId, operator SubscriberId, member SubscriberId) error
}

// OnMemoryGroupRepository is an in-memory GroupRepository, safe for
// concurrent use.
type OnMemoryGroupRepository struct {
	mu sync.RWMutex

	// byId indexes groups by channel id, then by subscriber id.
	byId map[ChannelId]map[SubscriberId]*Group

	// members indexes memberships by channel id, then group subscriber
	// id, then member subscriber id.
	members map[ChannelId]map[SubscriberId]map[SubscriberId]GroupMembership
}

// NewOnMemoryGroupRepository constructs an empty OnMemoryGroupRepository.
func NewOnMemoryGroupRepository() *OnMemoryGroupRepository {
	return &OnMemoryGroupRepository{
		byId:    map[ChannelId]map[SubscriberId]*Group{},
		members: map[ChannelId]map[SubscriberId]map[SubscriberId]GroupMembership{},
	}
}

// CreateGroup implements GroupRepository.
func (r *OnMemoryGroupRepository) CreateGroup(ctx context.Context, group *Group) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	members, ok := r.byId[group.ChannelId]
	if !ok {
		members = map[SubscriberId]*Group{}
		r.byId[group.ChannelId] = members
	}
	if _, exists := members[group.SubscriberId]; exists {
		return ErrGroupExists
	}
	// Store a copy so later caller-side mutation of the argument cannot
	// alias the repository's state.
	stored := *group
	members[group.SubscriberId] = &stored
	return nil
}

// DeleteGroup implements GroupRepository.
func (r *OnMemoryGroupRepository) DeleteGroup(ctx context.Context, channelId ChannelId, subscriberId SubscriberId) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if members, ok := r.byId[channelId]; ok {
		delete(members, subscriberId)
		if len(members) == 0 {
			delete(r.byId, channelId)
		}
	}
	if groups, ok := r.members[channelId]; ok {
		delete(groups, subscriberId)
		if len(groups) == 0 {
			delete(r.members, channelId)
		}
	}
	return nil
}

// GetGroupsByChannelId implements GroupRepository.
func (r *OnMemoryGroupRepository) GetGroupsByChannelId(ctx context.Context, channelId ChannelId) ([]*Group, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	members := r.byId[channelId]
	groups := make([]*Group, 0, len(members))
	for _, g := range members {
		copied := *g
		groups = append(groups, &copied)
	}
	slices.SortFunc(groups, func(a, b *Group) int {
		switch {
		case a.SubscriberId < b.SubscriberId:
			return -1
		case a.SubscriberId > b.SubscriberId:
			return 1
		default:
			return 0
		}
	})
	return groups, nil
}

// GetGroupBySubscriberId implements GroupRepository.
func (r *OnMemoryGroupRepository) GetGroupBySubscriberId(ctx context.Context, channelId ChannelId, subscriberId SubscriberId) (*Group, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.byId[channelId][subscriberId]
	if !ok {
		return nil, nil
	}
	copied := *g
	return &copied, nil
}

// JoinGroup implements GroupRepository.
func (r *OnMemoryGroupRepository) JoinGroup(ctx context.Context, channelId ChannelId, groupId SubscriberId, operator SubscriberId, member SubscriberId, role GroupRole) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byId[channelId][groupId]; !ok {
		return ErrGroupNotFound
	}
	groups, ok := r.members[channelId]
	if !ok {
		groups = map[SubscriberId]map[SubscriberId]GroupMembership{}
		r.members[channelId] = groups
	}
	members, ok := groups[groupId]
	if !ok {
		members = map[SubscriberId]GroupMembership{}
		groups[groupId] = members
	}
	if existing, ok := members[member]; ok && existing.Role == role {
		// A repetitive join with an identical (member, role, group) tuple
		// is a no-op.
		return nil
	}
	// A re-join with a different role updates the membership — a
	// promotion when the new role is lower-numbered, a demotion when
	// higher-numbered.
	members[member] = GroupMembership{
		ChannelId: channelId,
		GroupId:   groupId,
		Member:    member,
		Role:      role,
		Operator:  operator,
	}
	return nil
}

// LeaveGroup implements GroupRepository.
func (r *OnMemoryGroupRepository) LeaveGroup(ctx context.Context, channelId ChannelId, groupId SubscriberId, operator SubscriberId, member SubscriberId) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if groups, ok := r.members[channelId]; ok {
		if members, ok := groups[groupId]; ok {
			delete(members, member)
			if len(members) == 0 {
				delete(groups, groupId)
				if len(groups) == 0 {
					delete(r.members, channelId)
				}
			}
		}
	}
	return nil
}

var _ GroupRepository = (*OnMemoryGroupRepository)(nil)
