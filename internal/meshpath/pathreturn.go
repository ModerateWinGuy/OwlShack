// Package meshpath builds the PATH packets that teach a peer its route back to us. Both the
// companion (DM path returns) and the repeater client (reciprocal returns) need them.
package meshpath

import (
	"fmt"

	meshcore "github.com/OwlShack/meshcore-go"
)

// BuildReturn is the firmware's createPathReturn, refusing what it refuses: a path not matching
// pathLenByte, or path and extra past MAX_COMBINED_PATH (Mesh.cpp:452).
func BuildReturn(selfPubKey [meshcore.PubKeySize]byte, destPubKey, sharedSecret, inPath []byte, pathLenByte, extraType byte, extraData []byte) (*meshcore.Packet, error) {
	plain, err := (&meshcore.PathPayload{PathLength: pathLenByte, Path: inPath, ExtraType: extraType, Extra: extraData}).ToBytes()
	if err != nil {
		return nil, err
	}
	encrypted, err := meshcore.EncryptThenMAC(sharedSecret, plain)
	if err != nil {
		return nil, fmt.Errorf("encrypting path return: %w", err)
	}

	payload := make([]byte, 0, 2*meshcore.PathHashSize+len(encrypted))
	payload = append(payload, destPubKey[:meshcore.PathHashSize]...)
	payload = append(payload, selfPubKey[:meshcore.PathHashSize]...)
	payload = append(payload, encrypted...)

	return &meshcore.Packet{
		Header:     meshcore.MakeHeader(meshcore.RouteTypeFlood, meshcore.PayloadTypePath, 0),
		PathLength: meshcore.MakePathLen(meshcore.PathHashSize, 0),
		Payload:    payload,
	}, nil
}

// Direct re-points a return packet down a known route, as the firmware's sendDirect does.
func Direct(pkt *meshcore.Packet, path []byte, hashSize uint8) {
	if hashSize == 0 {
		hashSize = 1
	}
	pkt.Header = meshcore.MakeHeader(meshcore.RouteTypeDirect, meshcore.PayloadTypePath, 0)
	pkt.Path = path
	pkt.PathLength = meshcore.MakePathLen(hashSize, uint8(len(path)/int(hashSize)))
}
