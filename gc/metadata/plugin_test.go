package metadata

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"runtime/pprof"
	"sort"
	"testing"
	"time"

	"github.com/boltdb/bolt"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/content/local"
	"github.com/containerd/containerd/gc"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/metadata"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshot"
	"github.com/containerd/containerd/snapshot/naive"
	"github.com/gogo/protobuf/types"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

func TestMetadataCollector(t *testing.T) {
	db, cs, sn, cleanup := newStores(t)
	defer cleanup()

	objects := []object{
		blob(bytesFor(1), true),
		blob(bytesFor(2), false),
		blob(bytesFor(3), true),
		blob(bytesFor(4), false, "containerd.io/gc.root", time.Now().String()),
		newSnapshot("1", "", false, false),
		newSnapshot("2", "1", false, false),
		newSnapshot("3", "2", false, false),
		newSnapshot("4", "3", false, false),
		newSnapshot("5", "3", false, true),
		container("1", "4"),
		image("image-1", digestFor(2)),
	}

	ctx := context.Background()

	collector, err := NewMetadataCollector(db)
	if err != nil {
		t.Fatal(err)
	}

	var remaining []gc.Node

	collector.MLock(ctx)
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, obj := range objects {
			node, err := create(obj, tx, cs, sn)
			if err != nil {
				return err
			}
			if node != nil {
				remaining = append(remaining, *node)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("Creation failed: %+v", err)
	}
	collector.MUnlock()

	if err := collector.Trigger(ctx); err != nil {
		t.Fatal(err)
	}

	var actual []gc.Node

	if err := db.View(func(tx *bolt.Tx) error {
		nodeC := make(chan gc.Node)
		var scanErr error
		go func() {
			defer close(nodeC)
			scanErr = metadata.ScanAll(ctx, tx, nodeC)
		}()
		for node := range nodeC {
			actual = append(actual, node)
		}
		return scanErr
	}); err != nil {
		t.Fatal(err)
	}

	checkNodesEqual(t, actual, remaining)
}

func BenchmarkTrigger10(b *testing.B) {
	benchmarkTrigger(b, 10)
}

func BenchmarkTrigger100(b *testing.B) {
	benchmarkTrigger(b, 100)
}

func BenchmarkTrigger1000(b *testing.B) {
	benchmarkTrigger(b, 1000)
}

func BenchmarkTrigger10000(b *testing.B) {
	benchmarkTrigger(b, 10000)
}

func benchmarkTrigger(b *testing.B, n int) {
	db, cs, sn, cleanup := newStores(b)
	defer cleanup()

	objects := []object{}

	// TODO: Allow max to be configurable
	for i := 0; i < n; i++ {
		objects = append(objects,
			blob(bytesFor(int64(i)), false),
			image(fmt.Sprintf("image-%d", i), digestFor(int64(i))),
		)
		lastSnapshot := 9
		for j := 0; j <= lastSnapshot; j++ {
			var parent string
			key := fmt.Sprintf("snapshot-%d-%d", i, j)
			if j > 0 {
				parent = fmt.Sprintf("snapshot-%d-%d", i, j-1)
			}
			objects = append(objects, newSnapshot(key, parent, false, false))
		}
		objects = append(objects, container(fmt.Sprintf("container-%d", i), fmt.Sprintf("snapshot-%d-%d", i, lastSnapshot)))

	}

	// TODO: Create set of objects for removal

	ctx := context.Background()

	collector, err := NewMetadataCollector(db)
	if err != nil {
		b.Fatal(err)
	}

	var remaining []gc.Node

	collector.MLock(ctx)
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, obj := range objects {
			node, err := create(obj, tx, cs, sn)
			if err != nil {
				return err
			}
			if node != nil {
				remaining = append(remaining, *node)
			}
		}
		return nil
	}); err != nil {
		b.Fatalf("Creation failed: %+v", err)
	}
	collector.MUnlock()

	// TODO: reset benchmark
	b.ResetTimer()
	//b.StopTimer()

	labels := pprof.Labels("worker", "trigger")
	pprof.Do(ctx, labels, func(ctx context.Context) {
		for i := 0; i < b.N; i++ {

			// TODO: Add removal objects

			//b.StartTimer()

			if err := collector.Trigger(ctx); err != nil {
				b.Fatal(err)
			}

			//b.StopTimer()

			//var actual []gc.Node

			//if err := db.View(func(tx *bolt.Tx) error {
			//	nodeC := make(chan gc.Node)
			//	var scanErr error
			//	go func() {
			//		defer close(nodeC)
			//		scanErr = metadata.ScanAll(ctx, tx, nodeC)
			//	}()
			//	for node := range nodeC {
			//		actual = append(actual, node)
			//	}
			//	return scanErr
			//}); err != nil {
			//	t.Fatal(err)
			//}

			//checkNodesEqual(t, actual, remaining)
		}
	})
}

func bytesFor(i int64) []byte {
	r := rand.New(rand.NewSource(i))
	var b [256]byte
	_, err := r.Read(b[:])
	if err != nil {
		panic(err)
	}
	return b[:]
}

func digestFor(i int64) digest.Digest {
	r := rand.New(rand.NewSource(i))
	dgstr := digest.SHA256.Digester()
	_, err := io.Copy(dgstr.Hash(), io.LimitReader(r, 256))
	if err != nil {
		panic(err)
	}
	return dgstr.Digest()
}

type object struct {
	data    interface{}
	removed bool
	labels  map[string]string
}

func create(obj object, tx *bolt.Tx, cs content.Store, sn snapshot.Snapshotter) (*gc.Node, error) {
	var (
		node      *gc.Node
		namespace = "test"
		ctx       = namespaces.WithNamespace(context.Background(), namespace)
	)

	switch v := obj.data.(type) {
	case testContent:
		ctx := metadata.WithTransactionContext(ctx, tx)
		expected := digest.FromBytes(v.data)
		w, err := cs.Writer(ctx, "test-ref", int64(len(v.data)), expected)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create writer")
		}
		if _, err := w.Write(v.data); err != nil {
			return nil, errors.Wrap(err, "write blob failed")
		}
		if err := w.Commit(ctx, int64(len(v.data)), expected, content.WithLabels(obj.labels)); err != nil {
			return nil, errors.Wrap(err, "failed to commit blob")
		}
		if !obj.removed {
			node = &gc.Node{
				Type:      metadata.ResourceContent,
				Namespace: namespace,
				Key:       expected.String(),
			}
		}
	case testSnapshot:
		ctx := metadata.WithTransactionContext(ctx, tx)
		if v.active {
			_, err := sn.Prepare(ctx, v.key, v.parent, snapshot.WithLabels(obj.labels))
			if err != nil {
				return nil, err
			}
		} else {
			akey := fmt.Sprintf("%s-active", v.key)
			_, err := sn.Prepare(ctx, akey, v.parent)
			if err != nil {
				return nil, err
			}
			if err := sn.Commit(ctx, v.key, akey, snapshot.WithLabels(obj.labels)); err != nil {
				return nil, err
			}
		}
		if !obj.removed {
			node = &gc.Node{
				Type:      metadata.ResourceSnapshot,
				Namespace: namespace,
				Key:       fmt.Sprintf("naive/%s", v.key),
			}
		}
	case testImage:
		image := images.Image{
			Name:   v.name,
			Target: v.target,
			Labels: obj.labels,
		}
		_, err := metadata.NewImageStore(tx).Create(ctx, image)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create image")
		}
	case testContainer:
		container := containers.Container{
			ID:          v.id,
			SnapshotKey: v.snapshot,
			Snapshotter: "naive",
			Labels:      obj.labels,

			Runtime: containers.RuntimeInfo{
				Name: "testruntime",
			},
			Spec: &types.Any{},
		}
		_, err := metadata.NewContainerStore(tx).Create(ctx, container)
		if err != nil {
			return nil, err
		}
	}

	return node, nil
}

func blob(b []byte, r bool, l ...string) object {
	return object{
		data: testContent{
			data: b,
		},
		removed: r,
		labels:  labels(l),
	}
}

func image(n string, d digest.Digest, l ...string) object {
	return object{
		data: testImage{
			name: n,
			target: ocispec.Descriptor{
				MediaType: "irrelevant",
				Digest:    d,
				Size:      256,
			},
		},
		removed: false,
		labels:  labels(l),
	}
}

func newSnapshot(key, parent string, active, r bool, l ...string) object {
	return object{
		data: testSnapshot{
			key:    key,
			parent: parent,
			active: active,
		},
		removed: r,
		labels:  labels(l),
	}
}

func container(id, s string, l ...string) object {
	return object{
		data: testContainer{
			id:       id,
			snapshot: s,
		},
		removed: false,
		labels:  labels(l),
	}
}

func labels(l []string) map[string]string {
	if len(l)%2 != 0 {
		panic("labels must be provided as key and value")
	}
	ls := map[string]string{}
	for i := 0; i < len(l); i = i + 2 {
		ls[l[i]] = l[i+1]
	}
	return ls
}

type testContent struct {
	data []byte
}

type testSnapshot struct {
	key    string
	parent string
	active bool
}

type testImage struct {
	name   string
	target ocispec.Descriptor
}

type testContainer struct {
	id       string
	snapshot string
}

func newStores(t testing.TB) (*bolt.DB, content.Store, snapshot.Snapshotter, func()) {
	td, err := ioutil.TempDir("", "gc-test-")
	if err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(td, "meta.db"), 0644, nil)
	if err != nil {
		t.Fatal(err)
	}

	nsn, err := naive.NewSnapshotter(filepath.Join(td, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}

	lcs, err := local.NewStore(filepath.Join(td, "content"))
	if err != nil {
		t.Fatal(err)
	}

	return db, metadata.NewContentStore(db, lcs), metadata.NewSnapshotter(db, "naive", nsn), func() {
		os.RemoveAll(td)
	}
}

func checkNodesEqual(t *testing.T, n1, n2 []gc.Node) {
	sort.Sort(nodeList(n1))
	sort.Sort(nodeList(n2))

	if len(n1) != len(n2) {
		t.Errorf("Nodes do not match\n\tExpected:\n\t%v\n\tActual:\n\t%v", n2, n1)
		return
	}

	for i := range n1 {
		if n1[i] != n2[i] {
			t.Errorf("[%d] root does not match expected: expected %v, got %v", i, n2[i], n1[i])
		}
	}
}

type nodeList []gc.Node

func (nodes nodeList) Len() int {
	return len(nodes)
}

func (nodes nodeList) Less(i, j int) bool {
	if nodes[i].Type != nodes[j].Type {
		return nodes[i].Type < nodes[j].Type
	}
	if nodes[i].Namespace != nodes[j].Namespace {
		return nodes[i].Namespace < nodes[j].Namespace
	}
	return nodes[i].Key < nodes[j].Key
}

func (nodes nodeList) Swap(i, j int) {
	nodes[i], nodes[j] = nodes[j], nodes[i]
}
