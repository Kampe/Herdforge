package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type LoopMode string

const (
	LoopRunning LoopMode = "running"
	LoopHeld    LoopMode = "held"
	LoopOneShot LoopMode = "one-shot"
)

type LoopState struct {
	HoldIdentity
	Mode         LoopMode
	Goal         string
	Wakeup       string
	DeclaredGoal string
	DeclaredWake string
}

var ErrLoopContract = errors.New("lifecycle loop contract is incomplete")

func validLoopIdentity(id HoldIdentity) bool { return id.Scope == "lane" && id.valid() }

func (a *HoldAuthority) ConfigureLoop(ctx context.Context, id HoldIdentity, goal, wakeup string) error {
	if a == nil || !validLoopIdentity(id) || strings.TrimSpace(goal) == "" || strings.TrimSpace(wakeup) == "" {
		return ErrLoopContract
	}
	return a.withImmediate(ctx, "configure loop", func(ctx context.Context, q holdSQL) error {
		_, err := q.ExecContext(ctx, `INSERT INTO lifecycle_lane_loop(repository,owner,lane,mode,goal,wakeup,declared_goal,declared_wakeup) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(repository,owner,lane) DO UPDATE SET mode=excluded.mode,goal=excluded.goal,wakeup=excluded.wakeup,declared_goal=excluded.declared_goal,declared_wakeup=excluded.declared_wakeup`, id.Repository, id.Owner, id.Lane, string(LoopRunning), strings.TrimSpace(goal), strings.TrimSpace(wakeup), strings.TrimSpace(goal), strings.TrimSpace(wakeup))
		return err
	})
}

func (a *HoldAuthority) Loop(ctx context.Context, id HoldIdentity) (LoopState, error) {
	if a == nil || !validLoopIdentity(id) {
		return LoopState{}, ErrLoopContract
	}
	var state LoopState
	err := a.withImmediate(ctx, "read loop", func(ctx context.Context, q holdSQL) error {
		return q.QueryRowContext(ctx, `SELECT mode,goal,wakeup,declared_goal,declared_wakeup FROM lifecycle_lane_loop WHERE repository=? AND owner=? AND lane=?`, id.Repository, id.Owner, id.Lane).Scan(&state.Mode, &state.Goal, &state.Wakeup, &state.DeclaredGoal, &state.DeclaredWake)
	})
	if err != nil {
		return LoopState{}, fmt.Errorf("read loop: %w", err)
	}
	state.HoldIdentity = id
	return state, nil
}

// ReleaseAndRearm is the only lifecycle release path for a configured lane.
// Hold release and restoration of both standing values commit together.
func (a *HoldAuthority) ReleaseAndRearm(ctx context.Context, id HoldIdentity, actor, reason string, generation int64) (LoopState, error) {
	if a == nil || !validLoopIdentity(id) || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" {
		return LoopState{}, ErrLoopContract
	}
	var state LoopState
	err := a.withImmediate(ctx, "release and rearm", func(ctx context.Context, q holdSQL) error {
		var err error
		if _, err = a.releaseIn(ctx, q, id, actor, reason, "operator_release", generation); err != nil {
			return err
		}
		if err = q.QueryRowContext(ctx, `SELECT mode,declared_goal,declared_wakeup FROM lifecycle_lane_loop WHERE repository=? AND owner=? AND lane=?`, id.Repository, id.Owner, id.Lane).Scan(&state.Mode, &state.DeclaredGoal, &state.DeclaredWake); err != nil {
			return fmt.Errorf("read declared loop: %w", err)
		}
		if strings.TrimSpace(state.DeclaredGoal) == "" || strings.TrimSpace(state.DeclaredWake) == "" {
			return ErrLoopContract
		}
		_, err = q.ExecContext(ctx, `UPDATE lifecycle_lane_loop SET mode=?,goal=?,wakeup=? WHERE repository=? AND owner=? AND lane=?`, string(LoopRunning), state.DeclaredGoal, state.DeclaredWake, id.Repository, id.Owner, id.Lane)
		if err != nil {
			return err
		}
		state.Goal, state.Wakeup, state.Mode, state.HoldIdentity = state.DeclaredGoal, state.DeclaredWake, LoopRunning, id
		return nil
	})
	return state, err
}

func clearLoopIn(ctx context.Context, q holdSQL, id HoldIdentity) error {
	_, err := q.ExecContext(ctx, `UPDATE lifecycle_lane_loop SET mode=?,goal='',wakeup='' WHERE repository=? AND owner=? AND lane=?`, string(LoopHeld), id.Repository, id.Owner, id.Lane)
	return err
}
