package srv

import (
	"bytes"

	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"code.google.com/p/go-uuid/uuid"
	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/codahale/lunk"
	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/mailgun/log"
	"github.com/gravitational/teleport/Godeps/_workspace/src/golang.org/x/crypto/ssh"
	"github.com/gravitational/teleport/events"
)

type sessionRegistry struct {
	sync.Mutex
	sessions map[string]*session
	srv      *Server
}

func (s *sessionRegistry) newShell(sid string, sconn *ssh.ServerConn, ch ssh.Channel, req *ssh.Request, ctx *ctx) error {
	log.Infof("%v newShell(%v)", ctx, string(req.Payload))

	sess := newSession(sid, s)
	if err := sess.start(sconn, ch, ctx); err != nil {
		return err
	}
	s.sessions[sess.id] = sess
	log.Infof("%v created session: %v", ctx, sess.id)
	return nil
}

func (s *sessionRegistry) joinShell(sid string, sconn *ssh.ServerConn, ch ssh.Channel, req *ssh.Request, ctx *ctx) error {
	log.Infof("%v joinShell(%v)", ctx, string(req.Payload))
	s.Lock()
	defer s.Unlock()

	sess, found := s.findSession(sid)
	if !found {
		log.Infof("%v creating new session: %v", ctx, sid)
		return s.newShell(sid, sconn, ch, req, ctx)
	}
	log.Infof("%v joining session: %v", ctx, sess.id)
	sess.join(sconn, ch, req, ctx)
	return nil
}

func (s *sessionRegistry) leaveShell(sid, pid string) error {
	s.Lock()
	defer s.Unlock()

	sess, found := s.findSession(sid)
	if !found {
		return fmt.Errorf("session %v not found", sid)
	}
	if err := sess.leave(pid); err != nil {
		log.Errorf("failed to leave session: %v", err)
		return err
	}
	if len(sess.parties) != 0 {
		return nil
	}
	log.Infof("last party left %v, removing from server", sess)
	delete(s.sessions, sess.id)
	if err := sess.Close(); err != nil {
		log.Errorf("failed to close: %v", err)
		return err
	}
	return nil
}

func (s *sessionRegistry) broadcastResult(sid string, r execResult) error {
	s.Lock()
	defer s.Unlock()

	sess, found := s.findSession(sid)
	if !found {
		return fmt.Errorf("session %v not found", sid)
	}
	sess.broadcastResult(r)
	return nil
}

func (s *sessionRegistry) findSession(id string) (*session, bool) {
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	return sess, true
}

func newSessionRegistry(srv *Server) *sessionRegistry {
	return &sessionRegistry{
		srv:      srv,
		sessions: make(map[string]*session),
	}
}

type session struct {
	id      string
	eid     lunk.EventID
	r       *sessionRegistry
	writer  *multiWriter
	parties map[string]*party
	t       *term
}

func newSession(id string, r *sessionRegistry) *session {
	return &session{
		id:      id,
		r:       r,
		parties: make(map[string]*party),
		writer:  newMultiWriter(),
	}
}

func (s *session) Close() error {
	if s.t != nil {
		return s.t.Close()
	}
	return nil
}

func (s *session) start(sconn *ssh.ServerConn, ch ssh.Channel, ctx *ctx) error {
	s.eid = ctx.eid
	p := newParty(s, sconn, ch, ctx)
	if p.ctx.getTerm() != nil {
		s.t = p.ctx.getTerm()
		p.ctx.setTerm(nil)
	} else {
		var err error
		if s.t, err = newTerm(); err != nil {
			log.Infof("handleShell failed to create term: %v", err)
			return err
		}
	}
	cmd := exec.Command(s.r.srv.shell)
	// TODO(klizhentas) figure out linux user policy for launching shells,
	// what user and environment should we use to execute the shell? the simplest
	// answer is to use current user and env, however  what if we are root?
	cmd.Env = []string{"TERM=xterm", fmt.Sprintf("HOME=%v", os.Getenv("HOME"))}
	if err := s.t.run(cmd); err != nil {
		log.Infof("%v failed to start shell: %v", p.ctx, err)
		return err
	}
	log.Infof("%v starting shell input/output streaming", p.ctx)

	// Pipe session to shell and visa-versa capturing input and output
	out := &bytes.Buffer{}

	// TODO(klizhentas) implement capturing as a thread safe factored out feature
	// what is important is that writes and reads to buffer should be protected
	// out contains captured command output
	s.writer.addWriter("capture", out)

	s.addParty(p)

	go func() {
		written, err := io.Copy(s.writer, s.t.pty)
		log.Infof("%v shell to channel copy closed, bytes written: %v, err: %v",
			p.ctx, written, err)
	}()

	go func() {
		result, err := collectStatus(cmd, cmd.Wait())
		if err != nil {
			log.Errorf("%v wait failed: %v", p.ctx, err)
			s.r.srv.emit(ctx.eid, events.NewShell(sconn, s.r.srv.shell, out, -1, err))
		}
		if result != nil {
			log.Infof("%v result collected: %v", p.ctx, result)
			s.r.srv.emit(ctx.eid, events.NewShell(sconn, s.r.srv.shell, out, result.code, nil))
			s.r.broadcastResult(s.id, *result)
			log.Infof("%v result broadcasted", p.ctx)
		}
	}()

	return nil
}

func (s *session) broadcastResult(r execResult) {
	for _, p := range s.parties {
		p.ctx.sendResult(r)
	}
}

func (s *session) String() string {
	return fmt.Sprintf("session(id=%v, parties=%v)", s.id, len(s.parties))
}

func (s *session) leave(id string) error {
	p, ok := s.parties[id]
	if !ok {
		return fmt.Errorf("failed to find party: %v", id)
	}
	log.Infof("%v is leaving %v", p, s)
	delete(s.parties, p.id)
	s.writer.deleteWriter(p.id)
	return nil
}

func (s *session) addParty(p *party) {
	s.parties[p.id] = p
	s.writer.addWriter(p.id, p)
	p.ctx.addCloser(p)
	go func() {
		written, err := io.Copy(s.t.pty, p.ch)
		log.Infof("%v channel to shell copy closed, bytes written: %v, err: %v",
			p.ctx, written, err)
	}()
}

func (s *session) join(sconn *ssh.ServerConn, ch ssh.Channel, req *ssh.Request, ctx *ctx) (*party, error) {
	p := newParty(s, sconn, ch, ctx)
	s.addParty(p)
	return p, nil
}

type joinSubsys struct {
	srv *Server
	sid string
}

func parseJoinSubsys(name string, srv *Server) (*joinSubsys, error) {
	return &joinSubsys{
		srv: srv,
		sid: strings.TrimPrefix(name, "join:"),
	}, nil
}

func (j *joinSubsys) String() string {
	return fmt.Sprintf("joinSubsys(sid=%v)", j.sid)
}

func (j *joinSubsys) execute(sconn *ssh.ServerConn, ch ssh.Channel, req *ssh.Request, ctx *ctx) error {
	if err := j.srv.reg.joinShell(j.sid, sconn, ch, req, ctx); err != nil {
		log.Errorf("error: %v", err)
		return err
	}
	finished := make(chan bool)
	ctx.addCloser(closerFunc(func() error {
		close(finished)
		log.Infof("%v shutting down subsystem", ctx)
		return nil
	}))
	<-finished
	return nil
}

func newMultiWriter() *multiWriter {
	return &multiWriter{writers: make(map[string]io.Writer)}
}

type multiWriter struct {
	sync.RWMutex
	writers map[string]io.Writer
}

func (m *multiWriter) addWriter(id string, w io.Writer) {
	m.Lock()
	defer m.Unlock()
	m.writers[id] = w
}

func (m *multiWriter) deleteWriter(id string) {
	m.Lock()
	defer m.Unlock()
	delete(m.writers, id)
}

func (t *multiWriter) Write(p []byte) (n int, err error) {
	t.RLock()
	defer t.RUnlock()

	for _, w := range t.writers {
		n, err = w.Write(p)
		if err != nil {
			return
		}
		if n != len(p) {
			err = io.ErrShortWrite
			return
		}
	}
	return len(p), nil
}

func newParty(s *session, sconn *ssh.ServerConn, ch ssh.Channel, ctx *ctx) *party {
	return &party{
		id:    uuid.New(),
		sconn: sconn,
		ch:    ch,
		ctx:   ctx,
		s:     s,
	}
}

type party struct {
	id    string
	s     *session
	sconn *ssh.ServerConn
	ch    ssh.Channel
	ctx   *ctx
}

func (p *party) Write(bytes []byte) (int, error) {
	return p.ch.Write(bytes)
}

func (p *party) String() string {
	return fmt.Sprintf("%v party(id=%v)", p.ctx, p.id)
}

func (p *party) Close() error {
	log.Infof("%v closing", p)
	return p.s.r.leaveShell(p.s.id, p.id)
}
