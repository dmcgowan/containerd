package main

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"github.com/boltdb/bolt"
)

type Dumper interface {
	DumpBucket(name []byte, db *bolt.Bucket, level int) error
	DumpKey(key, value []byte, level int) error
}

func Dump(db *bolt.DB, dumper Dumper) error {
	return db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, bkt *bolt.Bucket) error {
			return dumper.DumpBucket(name, bkt, 0)
		})
	})
}

type dumper struct {
	buckets map[string]Dumper
	keys    map[string]valueFn
	any     Dumper
	key     func([]byte) string
	value   valueFn
}

func dumpBucket(bkt *bolt.Bucket, dumper Dumper, level int) error {
	bkt.ForEach(func(k, v []byte) error {
		if v == nil {
			return dumper.DumpBucket(k, bkt.Bucket(k), level)
		} else {
			return dumper.DumpKey(k, v, level)
		}
	})
	return nil
}

func (d dumper) DumpBucket(name []byte, bkt *bolt.Bucket, level int) error {
	bd, ok := d.buckets[string(name)]
	if !ok {
		if d.any == nil {
			fmt.Printf("%s[%s] - unknown bucket\n", strings.Repeat("  ", level), name)
			return nil
		}
		bd = d.any
	}
	fmt.Printf("%s[%s]\n", strings.Repeat("  ", level), name)

	return dumpBucket(bkt, bd, level+1)
}

func (d dumper) DumpKey(key, value []byte, level int) error {
	var (
		k, v string
	)
	if d.key != nil {
		k = d.key(key)
	} else {
		k = fmt.Sprintf("%s", key)
	}

	if d.keys != nil {
		fn, ok := d.keys[string(key)]
		if ok {
			v = fn(value)
		}
	}
	if v == "" {
		if d.value != nil {
			v = d.value(value)
		} else {
			v = fmt.Sprintf("%x", value)
		}
	}

	fmt.Printf("%s[%s]\n", strings.Repeat("  ", level), k)
	vindent := strings.Repeat("  ", level+1)
	fmt.Printf("%s%s\n", vindent, strings.Replace(v, "\n", "\n"+vindent, -1))

	return nil
}

type valueFn func(value []byte) string

func parentKey(b []byte) string {
	parent, i := binary.Uvarint(b)
	child, _ := binary.Uvarint(b[i+1:])
	return fmt.Sprintf("%d/%d", parent, child)
}

func strval(value []byte) string {
	return fmt.Sprintf("%s", value)
}

func intval(value []byte) string {
	n, _ := binary.Varint(value)
	return fmt.Sprintf("%d", n)
}

func uintval(value []byte) string {
	n, _ := binary.Uvarint(value)
	return fmt.Sprintf("%d", n)
}

func timeval(value []byte) string {
	var t time.Time
	if err := (&t).UnmarshalBinary(value); err != nil {
		return fmt.Sprintf("unknown time value (%x)", value)
	}

	return t.Format(time.RFC3339Nano)
}
