package repeater

import (
	"encoding/hex"
	"fmt"

	meshcore "github.com/meshcore-go/meshcore-go"
)

type PeerPathInfo struct {
	OutPath        string `json:"outPath"`
	Hops           int    `json:"hops"`
	HasPath        bool   `json:"hasPath"`
	DirectNeighbor bool   `json:"directNeighbor"`
	PathHashSize   int    `json:"pathHashSize"`
}

func (rm *Client) GetPeerPath(pubkeyHex string) (*PeerPathInfo, error) {
	pubkeyBytes, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid pubkey hex: %w", err)
	}

	peerIdentity, err := meshcore.NewIdentityFromBytes(pubkeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid pubkey: %w", err)
	}

	peer := rm.node.Peers().Lookup(peerIdentity.PublicKey())
	if peer == nil {
		return nil, fmt.Errorf("peer not found in peer table")
	}

	hashSize := int(peer.OutPathHashSize)
	if hashSize == 0 {
		hashSize = int(meshcore.PathHashSize)
	}

	info := &PeerPathInfo{PathHashSize: hashSize}
	if peer.OutPath != nil && len(peer.OutPath) == 0 {
		info.DirectNeighbor = true
		info.HasPath = true
		info.Hops = 0
	} else if len(peer.OutPath) > 0 {
		info.HasPath = true
		info.Hops = len(peer.OutPath) / hashSize
		info.OutPath = hex.EncodeToString(peer.OutPath)
	}
	return info, nil
}

func (rm *Client) ResetPeerPath(pubkeyHex string) error {
	pubkeyBytes, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return fmt.Errorf("invalid pubkey hex: %w", err)
	}

	peerIdentity, err := meshcore.NewIdentityFromBytes(pubkeyBytes)
	if err != nil {
		return fmt.Errorf("invalid pubkey: %w", err)
	}

	if !rm.node.Peers().ResetOutPath(peerIdentity.PublicKey()) {
		return fmt.Errorf("peer not found in peer table")
	}
	// The contact row has to be cleared too, or learnedRoute reads the old path straight back and
	// the reset does nothing.
	rm.persistOutPath(pubkeyBytes, nil, 0)
	rm.log.Debug("peer path reset", "peer", pubkeyHex[:12])
	return nil
}

func (rm *Client) SetPeerPath(pubkeyHex, pathHex string, pathHashSize int) error {
	pubkeyBytes, err := hex.DecodeString(pubkeyHex)
	if err != nil {
		return fmt.Errorf("invalid pubkey hex: %w", err)
	}

	peerIdentity, err := meshcore.NewIdentityFromBytes(pubkeyBytes)
	if err != nil {
		return fmt.Errorf("invalid pubkey: %w", err)
	}

	pathBytes, err := hex.DecodeString(pathHex)
	if err != nil {
		return fmt.Errorf("invalid path hex: %w", err)
	}

	if pathHashSize <= 0 {
		pathHashSize = int(meshcore.PathHashSize)
	}

	if !rm.node.Peers().SetOutPath(peerIdentity.PublicKey(), pathBytes, uint8(pathHashSize)) {
		return fmt.Errorf("peer not found in peer table")
	}
	rm.persistOutPath(pubkeyBytes, pathBytes, uint8(pathHashSize))
	rm.log.Debug("peer path set", "peer", pubkeyHex[:12], "path", pathHex)
	return nil
}
