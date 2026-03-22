package auth

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
)

var (
	ErrPermissionDenied = errors.New("permission denied")
	ErrUnknownUser      = errors.New("unknown run-as user")
)

type Identity struct {
	Username string
	UID      uint32
	GID      uint32
	IsRoot   bool
}

type ResolvedUser struct {
	Username string
	UID      uint32
	GID      uint32
}

type Authorizer struct {
	current Identity
}

func NewAuthorizer() (*Authorizer, error) {
	current, err := CurrentIdentity()
	if err != nil {
		return nil, err
	}

	return &Authorizer{current: current}, nil
}

func NewAuthorizerWithIdentity(identity Identity) *Authorizer {
	return &Authorizer{current: identity}
}

func CurrentIdentity() (Identity, error) {
	u, err := user.Current()
	if err != nil {
		return Identity{}, fmt.Errorf("resolve current user: %w", err)
	}

	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return Identity{}, fmt.Errorf("parse current uid: %w", err)
	}

	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return Identity{}, fmt.Errorf("parse current gid: %w", err)
	}

	return Identity{
		Username: u.Username,
		UID:      uint32(uid),
		GID:      uint32(gid),
		IsRoot:   os.Geteuid() == 0,
	}, nil
}

func (a *Authorizer) Current() Identity {
	return a.current
}

func (a *Authorizer) ResolveRunAs(runAs string) (ResolvedUser, error) {
	if runAs == "" {
		return ResolvedUser{
			Username: a.current.Username,
			UID:      a.current.UID,
			GID:      a.current.GID,
		}, nil
	}

	u, err := user.Lookup(runAs)
	if err != nil {
		return ResolvedUser{}, fmt.Errorf("%w: %s", ErrUnknownUser, runAs)
	}

	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return ResolvedUser{}, fmt.Errorf("parse run-as uid for %s: %w", runAs, err)
	}

	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return ResolvedUser{}, fmt.Errorf("parse run-as gid for %s: %w", runAs, err)
	}

	return ResolvedUser{
		Username: u.Username,
		UID:      uint32(uid),
		GID:      uint32(gid),
	}, nil
}

func (a *Authorizer) ValidateRunAs(runAs string) (ResolvedUser, error) {
	resolved, err := a.ResolveRunAs(runAs)
	if err != nil {
		return ResolvedUser{}, err
	}

	if a.current.IsRoot {
		return resolved, nil
	}

	if resolved.UID != a.current.UID {
		return ResolvedUser{}, fmt.Errorf(
			"%w: current user %q can only run as %q, requested %q",
			ErrPermissionDenied,
			a.current.Username,
			a.current.Username,
			resolved.Username,
		)
	}

	return resolved, nil
}
