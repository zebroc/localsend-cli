package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/zebroc/localsend-cli/internal/proto"
	"github.com/zebroc/localsend-cli/internal/ui"
)

// sessionTTL drops sessions a sender never finished, e.g. after it crashed.
const sessionTTL = 30 * time.Minute

type fileStatus int

const (
	statusPending fileStatus = iota
	statusInProgress
	statusDone
	statusFailed
)

type sessionFile struct {
	dto    proto.File
	token  string
	status fileStatus
}

type session struct {
	id       string
	senderIP string
	alias    string
	files    map[string]*sessionFile
	started  time.Time
	touched  time.Time
}

// sessionStore holds the active upload sessions.
//
// Unlike the official app we allow several senders at once: nothing here needs
// to ask a user, so blocking a second sender with 409 would only be a
// self-inflicted limit.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*session
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: map[string]*session{}}
}

// create registers a session for all offered files and returns the tokens the
// sender must use for the uploads.
func (s *sessionStore) create(ip, alias string, files map[string]proto.File) (*session, map[string]string) {
	sess := &session{
		id:       randomID(),
		senderIP: ip,
		alias:    alias,
		files:    make(map[string]*sessionFile, len(files)),
		started:  time.Now(),
		touched:  time.Now(),
	}
	tokens := make(map[string]string, len(files))
	for id, dto := range files {
		token := randomID()
		sess.files[id] = &sessionFile{dto: dto, token: token}
		tokens[id] = token
	}

	s.mu.Lock()
	s.sessions[sess.id] = sess
	s.mu.Unlock()
	return sess, tokens
}

// beginUpload validates an upload request and marks the file as in progress.
func (s *sessionStore) beginUpload(sessionID, fileID, token, ip string) (proto.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[sessionID]
	if !ok {
		return proto.File{}, fmt.Errorf("unknown session %s", sessionID)
	}
	if sess.senderIP != ip {
		return proto.File{}, fmt.Errorf("session %s belongs to %s", sessionID, sess.senderIP)
	}
	file, ok := sess.files[fileID]
	if !ok {
		return proto.File{}, fmt.Errorf("unknown file %s", fileID)
	}
	if file.token != token {
		return proto.File{}, fmt.Errorf("invalid token for file %s", fileID)
	}
	if file.status != statusPending {
		return proto.File{}, fmt.Errorf("file %s was already uploaded", fileID)
	}
	file.status = statusInProgress
	sess.touched = time.Now()
	return file.dto, nil
}

// finishUpload records the outcome and closes the session once every file
// reached a final state.
func (s *sessionStore) finishUpload(sessionID, fileID string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, exists := s.sessions[sessionID]
	if !exists {
		return
	}
	if file, exists := sess.files[fileID]; exists {
		if ok {
			file.status = statusDone
		} else {
			file.status = statusFailed
		}
	}
	sess.touched = time.Now()

	var done, failed int
	for _, file := range sess.files {
		switch file.status {
		case statusDone:
			done++
		case statusFailed:
			done++
			failed++
		}
	}
	if done < len(sess.files) {
		return
	}

	delete(s.sessions, sessionID)
	elapsed := ui.FormatDuration(time.Since(sess.started))
	if failed > 0 {
		ui.Warnf("transfer from %s finished with %d of %d file(s) failed", sess.alias, failed, len(sess.files))
		return
	}
	ui.Infof("transfer from %s complete: %d file(s) in %s", sess.alias, len(sess.files), elapsed)
}

// cancel drops a session on request of its sender.
func (s *sessionStore) cancel(sessionID, ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[sessionID]
	if !ok || sess.senderIP != ip {
		return false
	}
	delete(s.sessions, sessionID)
	return true
}

// reapLoop removes sessions that went stale.
func (s *sessionStore) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			for id, sess := range s.sessions {
				if time.Since(sess.touched) > sessionTTL {
					ui.Debugf("dropping stale session %s from %s", id, sess.alias)
					delete(s.sessions, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

// randomID returns a 128 bit random hex string, used for session IDs and tokens.
func randomID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on the platforms we support.
		panic(err)
	}
	return hex.EncodeToString(buf[:])
}
