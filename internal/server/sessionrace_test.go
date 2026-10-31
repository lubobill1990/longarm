package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/supervisor"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The races of the security review of programmable sessions.
func TestSessionRaces(t *testing.T) {
	e := newAuthzEnv(t)
	exe, _ := os.Executable()
	argv, env := []string{exe}, map[string]string{"LONGARM_TEST_SLEEP": "1"}
	sess := pb.NewSessionsClient(e.alice)
	ss := &sessionsServer{s: e.srv}
	sp := e.srv.Spaces.Shared()

	t.Run("start_never_returns_another_users_session", func(t *testing.T) {
		if _, err := sess.Start(cctx(t), &pb.StartSessionRequest{Name: "mine", Argv: argv, Env: env}); err != nil {
			t.Fatal(err)
		}
		// Bob got past sessionSpace before alice created the name: ensure
		// itself must refuse alice's session, with nothing of it.
		bob := context.WithValue(context.Background(), idKey{}, pki.Identity{Role: pki.RoleClient, Name: "bob-laptop", User: "bob"})
		for _, r := range []sessionReq{{argv: argv, env: env, strict: true}, {argv: argv, env: env}} {
			j, created, err := ss.ensure(bob, sp, r, "mine", "bob", "client/bob-laptop")
			if status.Code(err) != codes.AlreadyExists || created || j.Spec.ID != "" {
				t.Fatalf("bob ensure: %v %v %+v", err, created, j.Spec)
			}
		}
		if _, _, err := ss.ensure(bob, sp, sessionReq{noCreate: true}, "mine", "bob", "client/bob-laptop"); status.Code(err) != codes.NotFound {
			t.Fatalf("bob attach --no-create: %v", err)
		}
	})

	t.Run("concurrent_identical_starts_create_once", func(t *testing.T) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		created := 0
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, err := sess.Start(cctx(t), &pb.StartSessionRequest{Name: "dup", Argv: argv, Env: env})
				if err != nil {
					t.Error(err)
					return
				}
				if r.Created {
					mu.Lock()
					created++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if created != 1 {
			t.Fatalf("%d starts reported created", created)
		}
	})

	t.Run("stop_and_send_bound_to_their_generation", func(t *testing.T) {
		if _, err := sess.Start(cctx(t), &pb.StartSessionRequest{Name: "gen", Argv: argv, Env: env}); err != nil {
			t.Fatal(err)
		}
		old, _ := sp.Sessions.Get("gen")
		if _, err := sess.Kill(cctx(t), &pb.KillSessionRequest{Name: "gen"}); err != nil {
			t.Fatal(err)
		}
		r, err := sess.Start(cctx(t), &pb.StartSessionRequest{Name: "gen", Argv: argv, Env: env})
		if err != nil || !r.Created {
			t.Fatalf("replace: %v %v", r, err)
		}
		// steps of a stop / send of the old generation never reach the new one
		if _, err := sp.Sessions.KillGen(context.Background(), "gen", old.Spec.CreatedAt); !errors.Is(err, supervisor.ErrReplaced) {
			t.Fatalf("KillGen of the old generation: %v", err)
		}
		if _, err := ss.send(context.Background(), sp.Sessions, "gen", old.Spec.CreatedAt, []byte{3}, 0, "stop"); !errors.Is(err, errReplaced) {
			t.Fatalf("send to the old generation: %v", err)
		}
		if _, err := ss.waitTerminalGen(context.Background(), sp.Sessions, "gen", old.Spec.CreatedAt, time.Second); !errors.Is(err, errReplaced) {
			t.Fatalf("wait for the old generation: %v", err)
		}
		cur, _ := sp.Sessions.Get("gen")
		if cur.Status.State.Terminal() {
			t.Fatal("the new session was hit")
		}
		// its worktree name differs from the old generation's
		if jobs.SessionWorktreeKey("gen", old.Spec.CreatedAt) == jobs.SessionWorktreeKey("gen", cur.Spec.CreatedAt) {
			t.Fatal("generations share a worktree")
		}
		// a stop after the old one was replaced reports it as ended
		if j := replacedEnd(old); j.Status.State != jobs.Lost {
			t.Fatalf("replaced: %+v", j.Status)
		}
	})
}

func TestTailLimits(t *testing.T) {
	s := &Server{MaxTails: 3, MaxTailsPerUser: 2}
	a1, err := s.acquireTail("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquireTail("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquireTail("alice"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("per user: %v", err)
	}
	if _, err := s.acquireTail("bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquireTail("carol"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("total: %v", err)
	}
	a1()
	a1() // releasing twice frees one slot
	if _, err := s.acquireTail("carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquireTail("dave"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("total after release: %v", err)
	}
	unlimited := &Server{MaxTails: -1, MaxTailsPerUser: -1}
	for i := 0; i < 100; i++ {
		if _, err := unlimited.acquireTail("x"); err != nil {
			t.Fatal(err)
		}
	}
}

// A follow of a file that never appears ends (NOT_FOUND) instead of
// holding its helper forever.
func TestTailAbsentTimeout(t *testing.T) {
	tailPoll, tailAbsentTimeout = 10*time.Millisecond, 100*time.Millisecond
	defer func() { tailPoll, tailAbsentTimeout = 250*time.Millisecond, time.Hour }()
	err := tailFile(context.Background(), filepath.Join(t.TempDir(), "never"), 0, true, (&tailSink{}).send)
	if status.Code(err) != codes.NotFound {
		t.Fatalf("absent: %v", err)
	}
}
