package meshpath

import (
	"bytes"
	"testing"

	meshcore "github.com/OwlShack/meshcore-go"
)

func secret() []byte {
	s := make([]byte, 32)
	for i := range s {
		s[i] = byte(i + 1)
	}
	return s
}

// Two returns over the same route must differ: AES-ECB has no IV, so identical plaintext gives
// identical bytes and a relay's seen-table drops the second (Mesh.cpp:472-477 salts for this).
func TestBuildReturn_NoExtraIsSaltedUnique(t *testing.T) {
	t.Parallel()

	var self [meshcore.PubKeySize]byte
	dest := bytes.Repeat([]byte{0xAB}, 32)
	inPath := []byte{0x11, 0x22}

	a, err := BuildReturn(self, dest, secret(), inPath, 0x02, 0, nil)
	if err != nil {
		t.Fatalf("BuildReturn: %v", err)
	}
	b, err := BuildReturn(self, dest, secret(), inPath, 0x02, 0, nil)
	if err != nil {
		t.Fatalf("BuildReturn: %v", err)
	}
	if bytes.Equal(a.Payload, b.Payload) {
		t.Error("two extra-less path returns encoded identically, so relays would drop the second")
	}
}

// With a real extra payload the bytes are determined by it, and no salt is added.
func TestBuildReturn_WithExtraIsDeterministic(t *testing.T) {
	t.Parallel()

	var self [meshcore.PubKeySize]byte
	dest := bytes.Repeat([]byte{0xAB}, 32)
	extra := []byte{1, 2, 3, 4}

	a, _ := BuildReturn(self, dest, secret(), []byte{0x11}, 0x01, meshcore.PayloadTypeAck, extra)
	b, _ := BuildReturn(self, dest, secret(), []byte{0x11}, 0x01, meshcore.PayloadTypeAck, extra)
	if !bytes.Equal(a.Payload, b.Payload) {
		t.Error("an ack-carrying path return must not be salted: the ack is what makes it unique")
	}
}

func TestDirect_FramesTheRoute(t *testing.T) {
	t.Parallel()

	pkt := &meshcore.Packet{}
	Direct(pkt, []byte{0xaa, 0xbb, 0xcc, 0xdd}, 2)

	if !pkt.IsRouteDirect() {
		t.Error("Direct must set the direct route type")
	}
	if got := pkt.PathHashCount(); got != 2 {
		t.Errorf("hop count = %d, want 2 (4 bytes at 2 bytes per hash)", got)
	}
	if got := pkt.PathHashSize(); got != 2 {
		t.Errorf("hash size = %d, want 2", got)
	}
}

// createPathReturn's plaintext is [path_len][path][extra_type][extra] (Mesh.cpp:466-474), and it gives up on what does not fit or does not match.
func TestBuildReturn_FirmwareLayout(t *testing.T) {
	t.Parallel()

	var self [meshcore.PubKeySize]byte
	self[0] = 0x5E
	dest := bytes.Repeat([]byte{0xAB}, 32)
	for _, size := range []uint8{1, 2, 3} {
		path := bytes.Repeat([]byte{0x11, 0x22, 0x33}, 2)[:2*size]
		pathLen := meshcore.MakePathLen(size, 2)
		pkt, err := BuildReturn(self, dest, secret(), path, pathLen, meshcore.PayloadTypeAck, []byte{1, 2, 3, 4, 5, 6})
		if err != nil {
			t.Fatalf("%d-byte hashes: %v", size, err)
		}
		if pkt.Payload[0] != 0xAB || pkt.Payload[1] != 0x5E {
			t.Errorf("%d-byte hashes: dest/src = %x, want ab 5e", size, pkt.Payload[:2])
		}
		p, _ := meshcore.PathFromBytes(pkt.Payload)
		want := append(append([]byte{pathLen}, path...), meshcore.PayloadTypeAck, 1, 2, 3, 4, 5, 6)
		if plain := p.Decrypt(secret()); !bytes.Equal(plain[:len(want)], want) || !bytes.Equal(plain[len(want):], make([]byte, len(plain)-len(want))) {
			t.Errorf("%d-byte hashes: plaintext %x, want %x then zero padding", size, plain, want)
		}
	}

	if _, err := BuildReturn(self, dest, secret(), []byte{0x11}, meshcore.MakePathLen(1, 2), 0, nil); err == nil {
		t.Error("a path shorter than its path_len was sent; the firmware copies path_len's worth")
	}
	// 64 path bytes plus 98 of extra pass MAX_COMBINED_PATH (166 - 5).
	if _, err := BuildReturn(self, dest, secret(), make([]byte, 64), meshcore.MakePathLen(2, 32), meshcore.PayloadTypeResponse, make([]byte, 98)); err == nil {
		t.Error("a return past MAX_COMBINED_PATH was built; the firmware's createPathReturn gives up")
	}
	if _, err := BuildReturn(self, dest, secret(), make([]byte, 64), meshcore.MakePathLen(2, 32), meshcore.PayloadTypeResponse, make([]byte, 97)); err != nil {
		t.Errorf("a return exactly at MAX_COMBINED_PATH was refused: %v", err)
	}
}
