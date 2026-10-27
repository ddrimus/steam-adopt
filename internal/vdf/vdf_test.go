package vdf

import (
	"bytes"
	"errors"
	"testing"
)

// Build a shortcuts.vdf with one entry by hand, the way Steam writes it
func sample() []byte {
	var b bytes.Buffer
	b.WriteString("\x00shortcuts\x00")
	b.WriteString("\x000\x00")
	b.WriteString("\x02appid\x00\xfc\xd4\x04\xac")
	b.WriteString("\x01AppName\x00My Game\x00")
	b.WriteString("\x01Exe\x00\"/games/My Game/game.exe\"\x00")
	b.WriteString("\x07LastPlayTime\x00\x01\x02\x03\x04\x05\x06\x07\x08")
	b.WriteString("\x00tags\x00\x08")
	b.WriteString("\x08")
	b.WriteString("\x08")
	b.WriteString("\x08")
	return b.Bytes()
}

// Test that an unchanged file is written back byte for byte
func TestRoundTrip(t *testing.T) {
	data := sample()
	o, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := Marshal(o); !bytes.Equal(got, data) {
		t.Fatalf("round trip changed the data:\n got %q\nwant %q", got, data)
	}

	sc, _ := o.Get("shortcuts")
	entry, _ := sc.(*Object).Get("0")
	e := entry.(*Object)
	if got := e.Keys(); len(got) != 5 || got[0] != "appid" || got[4] != "tags" {
		t.Errorf("keys out of order: %v", got)
	}
	if v, _ := e.Get("appid"); v != int32(-1408969476) {
		t.Errorf("appid = %v (%T)", v, v)
	}
	if v, _ := e.Get("LastPlayTime"); v != uint64(0x0807060504030201) {
		t.Errorf("LastPlayTime = %v (%T)", v, v)
	}
	if s, ok := e.String("AppName"); !ok || s != "My Game" {
		t.Errorf("AppName = %q", s)
	}
}

// Test that setting an existing key keeps its position
func TestSetKeepsPosition(t *testing.T) {
	o := NewObject()
	o.Set("a", "1")
	o.Set("b", int32(2))
	o.Set("a", "3")
	o.Set("c", NewObject())
	want := "\x01a\x003\x00\x02b\x00\x02\x00\x00\x00\x00c\x00\x08\x08"
	if got := string(Marshal(o)); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Test that bytes that aren't valid UTF-8 survive a round trip
func TestInvalidUTF8Survives(t *testing.T) {
	data := []byte("\x01k\x00\xff\xfe\x00\x08")
	o, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(Marshal(o), data) {
		t.Fatal("bytes that aren't valid UTF-8 were changed")
	}
}

// Test that damaged data is refused instead of misread
func TestErrors(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":           {},
		"no end tag":      []byte("\x01k\x00v\x00"),
		"cut key":         []byte("\x01key"),
		"cut int":         []byte("\x02k\x00\x01\x02"),
		"cut uint64":      []byte("\x07k\x00\x01\x02\x03"),
		"unclosed object": []byte("\x00o\x00\x01k\x00v\x00"),
	} {
		if _, err := Unmarshal(data); !errors.Is(err, ErrTruncated) {
			t.Errorf("%s: got %v, want ErrTruncated", name, err)
		}
	}
	if _, err := Unmarshal([]byte("\x05k\x00\x08")); err == nil {
		t.Error("unknown type tag accepted")
	}
	deep := bytes.Repeat([]byte("\x00k\x00"), maxDepth+2)
	if _, err := Unmarshal(deep); err == nil || errors.Is(err, ErrTruncated) {
		t.Errorf("deep nesting: got %v, want depth error", err)
	}
}

// Fuzz test that anything parsed is written back in a form that parses again to the same bytes
func FuzzVDF(f *testing.F) {
	f.Add([]byte("\x00shortcuts\x00\x000\x00\x01AppName\x00x\x00\x02appid\x00\x01\x02\x03\x04\x08\x08\x08"))
	f.Fuzz(func(t *testing.T, data []byte) {
		o, err := Unmarshal(data)
		if err != nil {
			return
		}
		m := Marshal(o)
		o2, err := Unmarshal(m)
		if err != nil {
			t.Fatalf("own output unreadable: %v", err)
		}
		if !bytes.Equal(Marshal(o2), m) {
			t.Fatal("not stable")
		}
	})
}
