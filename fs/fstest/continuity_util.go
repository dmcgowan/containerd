package fstest

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/stevvooe/continuity"
)

type resourceUpdate struct {
	Original continuity.Resource
	Updated  continuity.Resource
}

func typeString(r continuity.Resource) string {
	switch r.(type) {
	case continuity.RegularFile:
		return "file"
	case continuity.Directory:
		return "directory"
	case continuity.SymLink:
		return "symlink"
	case continuity.NamedPipe:
		return "namedpipe"
	case continuity.Device:
		return "device"
	default:
		return "unknown"
	}
}

func combineMap(m map[string]string) (r string) {
	p := make([]string, 0, len(m))
	for k, v := range m {
		p = append(p, fmt.Sprintf("%s: %s", k, v))
	}

	if len(p) > 0 {
		r = "(" + strings.Join(p, ", ") + ")"
	}
	return
}

func (u resourceUpdate) String() string {
	old := map[string]string{}
	new := map[string]string{}
	if u.Original.Mode() != u.Updated.Mode() {
		old["mode"] = fmt.Sprintf("%s", u.Original.Mode())
		new["mode"] = fmt.Sprintf("%s", u.Updated.Mode())
	}
	if u.Original.UID() != u.Updated.UID() {
		old["uid"] = fmt.Sprintf("%s", u.Original.UID())
		new["uid"] = fmt.Sprintf("%s", u.Updated.UID())

	}
	if u.Original.GID() != u.Updated.GID() {
		old["gid"] = fmt.Sprintf("%s", u.Original.GID())
		new["gid"] = fmt.Sprintf("%s", u.Updated.GID())
	}
	if ot, ut := typeString(u.Original), typeString(u.Updated); ot != ut {
		old["type"] = ot
		new["type"] = ut
	} else if orf, ok := u.Original.(continuity.RegularFile); ok {
		urf := u.Updated.(continuity.RegularFile)
		if orf.Size() != urf.Size() {
			old["size"] = fmt.Sprintf("%s", orf.Size())
			new["size"] = fmt.Sprintf("%s", urf.Size())
		}
		p1 := orf.Paths()
		p2 := urf.Paths()
		if len(p1) != len(p2) {
			old["npaths"] = fmt.Sprintf("%d(%s)", len(p1), strings.Join(p1, ","))
			new["npaths"] = fmt.Sprintf("%d(%s)", len(p2), strings.Join(p2, ","))
		} else if len(p1) > 0 {
			for i := range p1 {
				if p1[i] != p2[i] {
					key := fmt.Sprintf("path[%d]", i)
					old[key] = p1[i]
					new[key] = p2[i]
				}
			}
		}

		d1 := orf.Digests()
		d2 := urf.Digests()
		if len(d1) != len(d2) {
			old["ndigests"] = fmt.Sprintf("%d", len(d1))
			new["ndigests"] = fmt.Sprintf("%d", len(d2))
		} else if len(d1) > 0 {
			for i := range d1 {
				if d1[i] != d2[i] {
					key := fmt.Sprintf("digest[%d]", i)
					old[key] = d1[i].String()
					new[key] = d2[i].String()
				}
			}
		}
	} else if osl, ok := u.Original.(continuity.SymLink); ok {
		usl := u.Updated.(continuity.SymLink)
		if osl.Target() != usl.Target() {
			old["target"] = osl.Target()
			new["target"] = usl.Target()
		}
	}

	return fmt.Sprintf("%s%s -> %s%s", u.Original.Path(), combineMap(old), u.Updated.Path(), combineMap(new))
}

type resourceListDifference struct {
	Additions []continuity.Resource
	Deletions []continuity.Resource
	Updates   []resourceUpdate
}

func (l resourceListDifference) HasDiff() bool {
	return len(l.Additions) > 0 || len(l.Deletions) > 0 || len(l.Updates) > 0
}

func (l resourceListDifference) String() string {
	buf := bytes.NewBuffer(nil)
	for _, add := range l.Additions {
		fmt.Fprintf(buf, "+ %s\n", add.Path())
	}
	for _, del := range l.Deletions {
		fmt.Fprintf(buf, "- %s\n", del.Path())
	}
	for _, upt := range l.Updates {
		fmt.Fprintf(buf, "~ %s\n", upt.String())
	}
	return string(buf.Bytes())
}

// diffManifest compares two resource lists and returns the list
// of adds updates and deletes, resource lists are not reordered
// before doing difference.
func diffResourceList(r1, r2 []continuity.Resource) resourceListDifference {
	i1 := 0
	i2 := 0
	var d resourceListDifference

	for i1 < len(r1) && i2 < len(r2) {
		p1 := r1[i1].Path()
		p2 := r2[i2].Path()
		switch {
		case p1 < p2:
			d.Deletions = append(d.Deletions, r1[i1])
			i1++
		case p1 == p2:
			if !compareResource(r1[i1], r2[i2]) {
				d.Updates = append(d.Updates, resourceUpdate{
					Original: r1[i1],
					Updated:  r2[i2],
				})
			}
			i1++
			i2++
		case p1 > p2:
			d.Additions = append(d.Additions, r2[i2])
			i2++
		}
	}

	for i1 < len(r1) {
		d.Deletions = append(d.Deletions, r1[i1])
		i1++

	}
	for i2 < len(r2) {
		d.Additions = append(d.Additions, r2[i2])
		i2++
	}

	return d
}

func compareResource(r1, r2 continuity.Resource) bool {
	if r1.Path() != r2.Path() {
		return false
	}
	if r1.Mode() != r2.Mode() {
		return false
	}
	if r1.UID() != r2.UID() {
		return false
	}
	if r1.GID() != r2.GID() {
		return false
	}

	// TODO(dmcgowan): Check if is XAttrer

	return compareResourceTypes(r1, r2)

}

func compareResourceTypes(r1, r2 continuity.Resource) bool {
	switch t1 := r1.(type) {
	case continuity.RegularFile:
		t2, ok := r2.(continuity.RegularFile)
		if !ok {
			return false
		}
		return compareRegularFile(t1, t2)
	case continuity.Directory:
		t2, ok := r2.(continuity.Directory)
		if !ok {
			return false
		}
		return compareDirectory(t1, t2)
	case continuity.SymLink:
		t2, ok := r2.(continuity.SymLink)
		if !ok {
			return false
		}
		return compareSymLink(t1, t2)
	case continuity.NamedPipe:
		t2, ok := r2.(continuity.NamedPipe)
		if !ok {
			return false
		}
		return compareNamedPipe(t1, t2)
	case continuity.Device:
		t2, ok := r2.(continuity.Device)
		if !ok {
			return false
		}
		return compareDevice(t1, t2)
	default:
		// TODO(dmcgowan): Should this panic?
		return r1 == r2
	}
}

func compareRegularFile(r1, r2 continuity.RegularFile) bool {
	if r1.Size() != r2.Size() {
		return false
	}
	p1 := r1.Paths()
	p2 := r2.Paths()
	if len(p1) != len(p2) {
		return false
	}
	for i := range p1 {
		if p1[i] != p2[i] {
			return false
		}
	}
	d1 := r1.Digests()
	d2 := r2.Digests()
	if len(d1) != len(d2) {
		return false
	}
	for i := range d1 {
		if d1[i] != d2[i] {
			return false
		}
	}

	return true
}

func compareSymLink(r1, r2 continuity.SymLink) bool {
	return r1.Target() == r2.Target()
}

func compareDirectory(r1, r2 continuity.Directory) bool {
	return true
}

func compareNamedPipe(r1, r2 continuity.NamedPipe) bool {
	return true
}

func compareDevice(r1, r2 continuity.Device) bool {
	return r1.Major() == r2.Major() && r1.Minor() == r2.Minor()
}
