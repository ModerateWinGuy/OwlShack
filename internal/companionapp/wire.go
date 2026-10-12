package companionapp

import (
	"encoding/binary"
	"errors"

	meshcore "github.com/OwlShack/meshcore-go"
	mc "github.com/OwlShack/meshcore-go/companion"
	"github.com/OwlShack/meshcore-go/node"
)

// appVersionCode is the FIRMWARE_VER_CODE whose features the app may use here: anon requests to non-contacts, not RUN_CLI_COMMAND.
const appVersionCode = 13

// maxChannels is the slots the app reads with GET_CHANNEL: the node's.
const maxChannels = node.DefaultMaxChannels

// maxContacts is MAX_CONTACTS; contacts here have no limit, so it reports the largest the frame carries.
const maxContacts = 510

func okFrame() []byte { return mc.OkResponse{}.ToBytes() }

func errFrame(code byte) []byte { return mc.ErrResponse{ErrorCode: code, HasErrorCode: true}.ToBytes() }

// refusal is the firmware's error code for a frame it will not take.
func refusal(err error) byte {
	if ce, ok := errors.AsType[*mc.CommandError](err); ok {
		return ce.ErrCode
	}
	return mc.ErrCodeUnsupportedCmd
}

func sentFrame(flood bool, tag, timeoutMs uint32) []byte {
	return mc.SentResponse{HasExtended: true, IsFlood: flood, Tag: tag, EstTimeout: timeoutMs}.ToBytes()
}

// capped cuts a message frame's text to fit, as the firmware does.
func capped(b []byte) []byte { return b[:min(len(b), mc.MaxFrameSize)] }

// pathLenByte is the firmware's path_len for a received packet: 0xFF when it came direct.
func pathLenByte(pkt *meshcore.Packet) byte {
	if pkt.IsRouteFlood() {
		return pkt.PathLength
	}
	return 0xFF
}

// legacyMsgFrame turns a V3 message frame into the pre-v3 one an older app asked for.
func legacyMsgFrame(v3 []byte) []byte {
	code := byte(mc.RespContactMsgRecv)
	if v3[0] == mc.RespChannelMsgRecvV3 {
		code = mc.RespChannelMsgRecv
	}
	return append([]byte{code}, v3[4:]...)
}

func prefix(pub [32]byte) [6]byte { return [6]byte(pub[:6]) }

// respServerLoginOK is the firmware's RESP_SERVER_LOGIN_OK.
const respServerLoginOK = 0

// loginFrame is the firmware's answer to a login reply, its tag first; a reply it cannot read is a failure.
func loginFrame(pub [32]byte, reply []byte) []byte {
	at := func(i int) byte {
		if i < len(reply) {
			return reply[i]
		}
		return 0
	}
	switch {
	case len(reply) >= 6 && string(reply[4:6]) == "OK":
		return mc.PushLoginSuccessResponse{PubKeyPrefix: prefix(pub)}.ToBytes()
	case len(reply) >= 5 && reply[4] == respServerLoginOK:
		return mc.PushLoginSuccessResponse{Permissions: at(6), PubKeyPrefix: prefix(pub), HasServerInfo: true,
			ServerTime: binary.LittleEndian.Uint32(reply[:4]), ACL: at(7), FirmwareLevel: at(12)}.ToBytes()
	}
	return mc.PushLoginFailResponse{PubKeyPrefix: prefix(pub)}.ToBytes()
}
