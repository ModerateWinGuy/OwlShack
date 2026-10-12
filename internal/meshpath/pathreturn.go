// Package meshpath builds the PATH packets that carry a reply and teach a peer its route back to us.
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
