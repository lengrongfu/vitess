/*
Copyright 2017 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreedto in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package consultopo

import (
	"fmt"
	"path"

	"github.com/hashicorp/consul/api"
	"golang.org/x/net/context"

	"github.com/youtube/vitess/go/vt/topo"
)

// consulLockDescriptor implements topo.LockDescriptor.
type consulLockDescriptor struct {
	s        *Server
	lockPath string
}

// Lock is part of the topo.Conn interface.
func (s *Server) Lock(ctx context.Context, dirPath, contents string) (topo.LockDescriptor, error) {
	// We list the directory first to make sure it exists.
	if _, err := s.ListDir(ctx, dirPath); err != nil {
		if err == topo.ErrNoNode {
			return nil, err
		}
		return nil, fmt.Errorf("cannot ListDir(%v) before locking: %v", dirPath, err)
	}

	lockPath := path.Join(s.root, dirPath, locksFilename)

	// Build the lock structure.
	l, err := s.client.LockOpts(&api.LockOptions{
		Key:   lockPath,
		Value: []byte(contents),
	})
	if err != nil {
		return nil, err
	}

	// Wait until we are the only ones in this client trying to
	// lock that path.
	s.mu.Lock()
	li, ok := s.locks[lockPath]
	for ok {
		// Unlock, wait for something to change.
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, convertError(ctx.Err())
		case <-li.done:
		}

		// The original locker is gone, try to get it again
		s.mu.Lock()
		li, ok = s.locks[lockPath]
	}
	li = &lockInstance{
		lock: l,
		done: make(chan struct{}),
	}
	s.locks[lockPath] = li
	s.mu.Unlock()

	// We are the only ones trying to lock now.
	// FIXME(alainjobart) We don't look at the 'lost' channel
	// returned here. We need to fix this in our code base, to add
	// APIs to make sure a lock is still held.
	_, err = l.Lock(ctx.Done())
	if err != nil {
		// Failed to lock, give up our slot in locks map.
		// Close the channel to unblock anyone else.
		s.mu.Lock()
		delete(s.locks, lockPath)
		s.mu.Unlock()
		close(li.done)

		return nil, err
	}

	// We got the lock, we're good.
	return &consulLockDescriptor{
		s:        s,
		lockPath: lockPath,
	}, nil
}

// Unlock is part of the topo.LockDescriptor interface.
func (ld *consulLockDescriptor) Unlock(ctx context.Context) error {
	return ld.s.unlock(ctx, ld.lockPath)
}

// unlock releases a lock acquired by Lock() on the given directory.
func (s *Server) unlock(ctx context.Context, lockPath string) error {
	s.mu.Lock()
	li, ok := s.locks[lockPath]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unlock: lock %v not held", lockPath)
	}

	// Try to unlock our lock. We will clean up our entry anyway.
	unlockErr := li.lock.Unlock()

	s.mu.Lock()
	delete(s.locks, lockPath)
	s.mu.Unlock()
	close(li.done)

	return unlockErr
}
