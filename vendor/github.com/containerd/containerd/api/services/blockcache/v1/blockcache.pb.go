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

// Hand-written protobuf message types for the BlockCache ttrpc service.
// Not auto-generated; avoids protoc toolchain dependency.
// Wire encoding uses standard protobuf field tags via google.golang.org/protobuf.

package blockcache

import "fmt"

// ── FillMessage ───────────────────────────────────────────────────────────────

// FillMessage is the single envelope type on both sides of the Fill stream.
type FillMessage struct {
	Hello  *Hello       `protobuf:"bytes,1,opt,name=hello,proto3" json:"hello,omitempty"`
	Fill   *FillRequest `protobuf:"bytes,2,opt,name=fill,proto3" json:"fill,omitempty"`
	Filled *Filled      `protobuf:"bytes,3,opt,name=filled,proto3" json:"filled,omitempty"`
	Error  *FillError   `protobuf:"bytes,4,opt,name=error,proto3" json:"error,omitempty"`
}

func (x *FillMessage) Reset()         { *x = FillMessage{} }
func (x *FillMessage) String() string { return fmt.Sprintf("%+v", *x) }
func (*FillMessage) ProtoMessage()    {}

// ── Hello ─────────────────────────────────────────────────────────────────────

// Hello identifies the block to fill; must be the first message on the stream.
type Hello struct {
	// Blockid is an opaque block identifier, typically a content digest
	// matching the blockid= mount option (e.g. "sha256:abc…").
	Blockid string `protobuf:"bytes,1,opt,name=blockid,proto3" json:"blockid,omitempty"`
}

func (x *Hello) Reset()         { *x = Hello{} }
func (x *Hello) String() string { return fmt.Sprintf("%+v", *x) }
func (*Hello) ProtoMessage()    {}

func (x *Hello) GetBlockid() string {
	if x != nil {
		return x.Blockid
	}
	return ""
}

// ── FillRequest ───────────────────────────────────────────────────────────────

// FillRequest asks the daemon to ensure [Offset, Offset+Length) is resident.
type FillRequest struct {
	Offset int64 `protobuf:"varint,1,opt,name=offset,proto3" json:"offset,omitempty"`
	Length int64 `protobuf:"varint,2,opt,name=length,proto3" json:"length,omitempty"`
}

func (x *FillRequest) Reset()         { *x = FillRequest{} }
func (x *FillRequest) String() string { return fmt.Sprintf("%+v", *x) }
func (*FillRequest) ProtoMessage()    {}

func (x *FillRequest) GetOffset() int64 {
	if x != nil {
		return x.Offset
	}
	return 0
}
func (x *FillRequest) GetLength() int64 {
	if x != nil {
		return x.Length
	}
	return 0
}

// ── Filled ────────────────────────────────────────────────────────────────────

// Filled reports which byte ranges are now resident in the backing file.
// Ranges are cumulative; the shim ORs them into its local page bitmap.
type Filled struct {
	Ranges []*ByteRange `protobuf:"bytes,1,rep,name=ranges,proto3" json:"ranges,omitempty"`
}

func (x *Filled) Reset()         { *x = Filled{} }
func (x *Filled) String() string { return fmt.Sprintf("%+v", *x) }
func (*Filled) ProtoMessage()    {}

func (x *Filled) GetRanges() []*ByteRange {
	if x != nil {
		return x.Ranges
	}
	return nil
}

// ── FillError ─────────────────────────────────────────────────────────────────

// FillError signals that a fill could not be completed.  The shim should
// propagate an I/O error to the waiting container read.
type FillError struct {
	Message string `protobuf:"bytes,1,opt,name=message,proto3" json:"message,omitempty"`
}

func (x *FillError) Reset()         { *x = FillError{} }
func (x *FillError) String() string { return fmt.Sprintf("%+v", *x) }
func (*FillError) ProtoMessage()    {}

func (x *FillError) GetMessage() string {
	if x != nil {
		return x.Message
	}
	return ""
}

// ── ByteRange ─────────────────────────────────────────────────────────────────

// ByteRange is a [Offset, Offset+Length) byte span.
type ByteRange struct {
	Offset int64 `protobuf:"varint,1,opt,name=offset,proto3" json:"offset,omitempty"`
	Length int64 `protobuf:"varint,2,opt,name=length,proto3" json:"length,omitempty"`
}

func (x *ByteRange) Reset()         { *x = ByteRange{} }
func (x *ByteRange) String() string { return fmt.Sprintf("%+v", *x) }
func (*ByteRange) ProtoMessage()    {}

func (x *ByteRange) GetOffset() int64 {
	if x != nil {
		return x.Offset
	}
	return 0
}
func (x *ByteRange) GetLength() int64 {
	if x != nil {
		return x.Length
	}
	return 0
}
