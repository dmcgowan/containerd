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

// Package manager is used to manage mounts in a bolt database, normally
// backed by a tempfs.
//
// The top level bucket name is the schema version. A structural,
// backwards incompatible change to the schema, such as this one, is
// expressed by moving to a new name rather than by migrating data
// within the old one: "v2" is a distinct bucket from "v1", so a binary
// which only understands "v1" neither reads nor writes anything under
// "v2", and a binary which only understands "v2" never touches "v1".
// Each simply creates its own bucket on first use.
//
// This means a rollback across this change requires no compatibility
// code of any kind: an older binary started against a database this
// package has written falls back to its own "v1" bucket exactly as it
// would against a brand new database, because "v1" was never modified.
// The mounts recorded only under "v2" are not something the older
// binary can restore, but the database is expected to live on
// transient storage (normally a tmpfs under the state directory) and
// every caller of Activate treats an unrecognized name as not yet
// activated and activates it again, so this is judged an acceptable
// cost rather than something to engineer around: a rollback attempted
// after the new binary has already run and mounted things is not
// something this store promises to preserve, only to not corrupt.
//
// The other direction, upgrading, is not left to leak: whatever "v1"
// still holds when a binary built with this schema starts is converted
// into "v2" and "v1" is deleted, so a routine operation such as
// deleting a container does not silently fail to release the mounts
// it made before the upgrade. This happens lazily, inside the first
// write transaction this package performs, rather than at open, so
// that a process which fails to start for an unrelated reason before
// ever touching a mount leaves "v1" untouched and a rollback to the
// older binary still works; once that first write transaction commits
// the conversion is permanent and rollback is no longer expected to
// preserve anything, matching the paragraph above. See migrateFromV1.
//
// Every mount the manager performs is recorded once, as a backing
// mount, and referenced by the activations using it: two activations
// which describe the same mount are backed by the same record and
// share the underlying filesystem, so it stays mounted while it backs
// either of them. This applies uniformly; a mount which cannot be
// shared with another activation still gets its own backing mount
// record, it is simply never looked up by another activation's mount
// parameters.
/*
Database schema

	v2
	╘══*namespace*
	   ├──mounts
	   │  ╘══*mount name*
	   │     ├──id : <varuint64>                  - Unique ID for mount (auto incrementing)
	   │     ├──createdat : <binary time>         - Created at
	   │     ├──updatedat : <binary time>         - Updated at
	   │     ├──lease : <string>                  - Lease
	   │     ├──complete : <bool>                 - Set once activation finished, absent while in flight
	   │     ├──active                            - Written as each mount in the chain is claimed
	   │     │  ╘══*order*
	   │     │     └──backedby : <varuint64>      - Backing mount for this position in the chain
	   │     ├──system
	   │     │  ╘══*order*
	   │     │     ├──type : <string>             - Mount type
	   │     │     ├──source : <string>           - Mount source
	   │     │     ├──target : <string>           - Mount target (relative to previous mount point)
	   │     │     └──options : <string>          - Comma separate options
	   │     └──labels
	   │        ╘══*key* : <string>               - Label value
	   ├──backing                                 - Mounts actually performed by the manager
	   │  ╘══*backing id (varuint64)*
	   │     ├──type : <string>                   - Mount type
	   │     ├──source : <string>                 - Mount source
	   │     ├──target : <string>                 - Mount target
	   │     ├──options : <string>                - NUL separated options
	   │     ├──mp : <string>                     - Mount point, empty until the mount succeeds
	   │     ├──mat : <binary time>               - Mounted at
	   │     └──refs
	   │        ╘══*mount name* : nil             - Activations referencing this backing mount
	   ├──backingindex                            - Dedup index over shareable backing mounts
	   │  ╘══*mount identity digest* : <varuint64>
	   ├──leases
	   │  ╘══*lease id*
	   │     ╘══*mount name*: nil
	   └──unmountq                                (CURRENTLY NOT USED, may remove)
	      └──*mount name + auto-increment*
	         ├──type : <string>                   - Mount type
	         ├──target : <string>                 - Path to check && unmount
	         ├──rm : <bool>                       - Whether to remove target after unmount
	         ├──dev : <string>                    - Device to check before unmount
	         ├──pid : <int>                       - Process to check and kill
	         ├──target : <string>                 - Path to unmount
	         ├──state : <enum>                    - (0 - unmounted, 1 - filesystem, 2 - device, 3 - process)
	         └─ order : <int>                     - Order in which was mounted, unmount high to low
*/
package manager

var (
	bucketKeyVersion = []byte("v2")
	// bucketKeyVersionV1 is the schema this package replaced. It is
	// never written by this package; it is only ever read, once, by
	// migrateFromV1.
	bucketKeyVersionV1 = []byte("v1")

	bucketKeyID           = []byte("id")
	bucketKeyMounts       = []byte("mounts")
	bucketKeyLeases       = []byte("leases")
	bucketKeyLease        = []byte("lease")
	bucketKeyActive       = []byte("active")
	bucketKeyComplete     = []byte("complete")
	bucketKeySystem       = []byte("system")
	bucketKeyType         = []byte("type")
	bucketKeySource       = []byte("source")
	bucketKeyTarget       = []byte("target")
	bucketKeyOptions      = []byte("options")
	bucketKeyMountedAt    = []byte("mat")
	bucketKeyMountPoint   = []byte("mp")
	bucketKeyLabels       = []byte("labels")
	bucketKeyBacking      = []byte("backing")
	bucketKeyBackingIndex = []byte("backingindex")
	bucketKeyBackedBy     = []byte("backedby")
	bucketKeyRefs         = []byte("refs")

	labelGCContainerBackRef = []byte("containerd.io/gc.bref.container")
	labelGCContentBackRef   = []byte("containerd.io/gc.bref.content")
	labelGCImageBackRef     = []byte("containerd.io/gc.bref.image")
	labelGCSnapBackRef      = []byte("containerd.io/gc.bref.snapshot.")
)
