/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package local

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/core/metadata/boltutil"
	errbolt "go.etcd.io/bbolt/errors"
	bolt "go.etcd.io/bbolt"
)

// Transactor is the database interface required by the indexed content store.
// It is satisfied by *bolt.DB directly and by *metadata.DB, allowing the store
// to share the metadata BoltDB in production while accepting a plain *bolt.DB
// in tests.
//
// The interface is intentionally identical to metadata.Transactor in
// core/metadata/bolt.go; defining it locally avoids an import cycle.
type Transactor interface {
	View(fn func(*bolt.Tx) error) error
	Update(fn func(*bolt.Tx) error) error
}

// view runs fn in a read-only bolt transaction.  If ctx already carries a
// transaction (via boltutil.WithTransaction) fn is called on that transaction
// directly; otherwise a new View transaction is opened on db.
func view(ctx context.Context, db Transactor, fn func(*bolt.Tx) error) error {
	tx, ok := boltutil.Transaction(ctx)
	if !ok {
		return db.View(fn)
	}
	return fn(tx)
}

// update runs fn in a writable bolt transaction.  If ctx already carries a
// writable transaction (via boltutil.WithTransaction) fn is called on that
// transaction directly; otherwise a new Update transaction is opened on db.
func update(ctx context.Context, db Transactor, fn func(*bolt.Tx) error) error {
	tx, ok := boltutil.Transaction(ctx)
	if !ok {
		return db.Update(fn)
	}
	if !tx.Writable() {
		return fmt.Errorf("content/index: read-only tx in context: %w",
			errbolt.ErrTxNotWritable)
	}
	return fn(tx)
}
