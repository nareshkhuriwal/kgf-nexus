// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package objstoretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/objstore"
)

// Memory is an in-process objstore.Store for unit tests. It passes the same
// conformance suite as the real backends (TestMemoryConformance), so a unit
// test written against it is testing the contract, not a convenient fiction.
type Memory struct {
	mu      sync.Mutex
	bucket  string
	exists  bool
	objects map[string]memObject
	// Now is the clock stamped on writes; tests move it to age objects.
	Now func() time.Time
}

type memObject struct {
	data []byte
	ct   string
	mod  time.Time
}

// NewMemory returns an existing, empty bucket.
func NewMemory(bucket string) *Memory {
	return &Memory{bucket: bucket, exists: true, objects: map[string]memObject{}, Now: time.Now}
}

// NewMissingMemory returns a store whose bucket does not exist yet.
func NewMissingMemory(bucket string) *Memory {
	m := NewMemory(bucket)
	m.exists = false
	return m
}

// SetModified back- or forward-dates an object (tests of age-based logic).
func (m *Memory) SetModified(key string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objects[key]; ok {
		o.mod = t
		m.objects[key] = o
	}
}

// Keys lists every key, sorted.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objects))
	for k := range m.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (m *Memory) Backend() string { return "memory" }
func (m *Memory) Bucket() string  { return m.bucket }

func (m *Memory) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.exists {
		return fmt.Errorf("memory: bucket %s does not exist", m.bucket)
	}
	return nil
}

func (m *Memory) Put(ctx context.Context, key string, body io.Reader, size int64, o objstore.PutOptions) error {
	if err := objstore.ValidateKey(key); err != nil {
		return err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("memory: read body: %w", err)
	}
	if size >= 0 && int64(len(data)) != size {
		return fmt.Errorf("memory: body was %d bytes, expected %d", len(data), size)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx); err != nil {
		return err
	}
	if _, ok := m.objects[key]; ok && o.IfAbsent {
		return objstore.ErrExists
	}
	ct := o.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	m.objects[key] = memObject{data: data, ct: ct, mod: m.Now()}
	return nil
}

func (m *Memory) Stat(ctx context.Context, key string) (objstore.ObjectInfo, error) {
	if key == "" {
		return objstore.ObjectInfo{}, errors.New("memory: empty key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx); err != nil {
		return objstore.ObjectInfo{}, err
	}
	o, ok := m.objects[key]
	if !ok {
		return objstore.ObjectInfo{}, objstore.ErrNotFound
	}
	return objstore.ObjectInfo{Key: key, Size: int64(len(o.data)), LastModified: o.mod, ContentType: o.ct}, nil
}

func (m *Memory) Get(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if key == "" {
		return nil, errors.New("memory: empty key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx); err != nil {
		return nil, err
	}
	o, ok := m.objects[key]
	if !ok {
		return nil, objstore.ErrNotFound
	}
	if off > int64(len(o.data)) {
		return nil, fmt.Errorf("memory: offset %d past the end of %s", off, key)
	}
	end := int64(len(o.data))
	if n >= 0 && off+n < end {
		end = off + n
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), o.data[off:end]...))), nil
}

func (m *Memory) List(ctx context.Context, prefix string, fn func(objstore.ObjectInfo) error) error {
	m.mu.Lock()
	if err := m.check(ctx); err != nil {
		m.mu.Unlock()
		return err
	}
	var infos []objstore.ObjectInfo
	for k, o := range m.objects {
		if strings.HasPrefix(k, prefix) {
			infos = append(infos, objstore.ObjectInfo{Key: k, Size: int64(len(o.data)), LastModified: o.mod, ContentType: o.ct})
		}
	}
	m.mu.Unlock()
	sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
	for _, i := range infos {
		if err := fn(i); err != nil {
			if errors.Is(err, objstore.ErrStop) {
				return nil
			}
			return err
		}
	}
	return nil
}

func (m *Memory) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("memory: refusing to delete an empty key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.check(ctx); err != nil {
		return err
	}
	delete(m.objects, key)
	return nil
}

func (m *Memory) BucketExists(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exists, nil
}

func (m *Memory) CreateBucket(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exists = true
	return nil
}

var _ objstore.Store = (*Memory)(nil)
