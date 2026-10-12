package companionapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	meshcore "github.com/OwlShack/meshcore-go"
	mc "github.com/OwlShack/meshcore-go/companion"
	"github.com/OwlShack/meshcore-go/node"

	"github.com/OwlShack/OwlShack/internal/buildinfo"
	"github.com/OwlShack/OwlShack/internal/client/repeater"
	"github.com/OwlShack/OwlShack/internal/config"
	"github.com/OwlShack/OwlShack/internal/modem"
	"github.com/OwlShack/OwlShack/internal/node/companion"
	"github.com/OwlShack/OwlShack/internal/store"
)

// reqTimeout is the floor on a remote request's wait; the client stretches it to the route's airtime.
const reqTimeout = 10 * time.Second

// clockSlack is how far the app's clock may be from ours and still count as the time we already have.
const clockSlack = 60 * time.Second

// Env is what the app is told about the radio and the host, refreshed on every reload.
type Env struct {
	Radio         modem.RadioInfo
	AirtimeFactor float64
	// Regions is the flood region list, which names the scope key the app sends with.
	Regions []string
	Stats   modem.StatsProvider
	// Tx is the shared radio's transmit counters, every node's sends and relays together.
	Tx      interface{ TxStats() node.TxStats }
	Started time.Time
	// QueueSize is how many messages are held while the app is away.
	QueueSize int
}

// Settings saves what the app changes on its companion, through the checks and reload the UI's saves take.
type Settings interface {
	UpdateCompanion(ctx context.Context, companionID int64, change func(*store.Companion) error) error
	SaveChannels(ctx context.Context, c *companion.Companion) error
}

// session is what the firmware keeps per app: the protocol version it speaks and the scope it chose for sends.
type session struct {
	appVer byte
	// scope overrides the companion's own choice when the app set one; nil leaves it alone.
	scope *companion.SendScope
}

// appSend is how a request the app makes goes out: in the app's scope, telling sent when it leaves.
func (c *conn) appSend(comp *companion.Companion, pub []byte, sent func(uint32, time.Duration, bool)) repeater.AppSend {
	return repeater.AppSend{Scope: comp.AppRegion(pub, c.session.scope), Sent: sent}
}

// handle answers one command. Only what is listed is served; everything else is refused as the firmware refuses a command it lacks.
func (c *conn) handle(d []byte) {
	s := c.srv
	comp := s.comp.Load()
	env := s.env.Load()
	ctx := context.Background()

	v, err := mc.ParseCommand(d)
	if err != nil {
		c.send(errFrame(refusal(err)))
		return
	}
	switch cmd := v.(type) {
	case mc.DeviceQueryCommand:
		c.session.appVer = cmd.AppTargetVersion
		c.send(mc.DeviceInfoResponse{FirmwareVersion: appVersionCode, MaxContacts: maxContacts, MaxChannels: maxChannels,
			FirmwareBuildDate: buildinfo.Date, Model: "OwlShack", FirmwareVersionStr: buildinfo.Version, PathHashMode: comp.PathHashSize() - 1}.ToBytes())

	case mc.AppStartCommand:
		c.send(c.selfInfo(comp, env).ToBytes())

	case mc.GetContactsCommand:
		contacts, err := s.st.Contacts.List(ctx, s.id)
		if err != nil {
			c.send(errFrame(mc.ErrCodeFileIoError))
			return
		}
		c.send(mc.ContactsStartResponse{Count: uint32(len(contacts)), HasCount: true}.ToBytes())
		var newest uint32
		for i := range contacts {
			k := storedContact(&contacts[i])
			if k.LastModified > cmd.Since {
				c.send(k.ToBytes())
				newest = max(newest, k.LastModified)
			}
		}
		c.send(mc.EndOfContactsResponse{MostRecentLastmod: newest}.ToBytes())

	case mc.GetContactByKeyCommand:
		k, ok := s.contact(cmd.PublicKey[:])
		if !ok {
			c.send(errFrame(mc.ErrCodeNotFound))
			return
		}
		c.send(storedContact(k).ToBytes())

	case mc.AddUpdateContactCommand:
		c.send(c.addUpdateContact(comp, cmd))

	case mc.RemoveContactCommand:
		if _, ok := s.contact(cmd.PublicKey[:]); !ok {
			c.send(errFrame(mc.ErrCodeNotFound))
			return
		}
		s.st.WriteSync(func() { err = s.st.Contacts.Delete(ctx, s.id, cmd.PublicKey[:]) })
		c.send(okOr(err, mc.ErrCodeFileIoError))

	case mc.ResetPathCommand:
		if _, ok := s.contact(cmd.PublicKey[:]); !ok {
			c.send(errFrame(mc.ErrCodeNotFound))
			return
		}
		c.send(okOr(comp.Repeaters().ResetPeerPath(hex.EncodeToString(cmd.PublicKey[:])), mc.ErrCodeNotFound))

	case mc.GetAdvertPathCommand:
		// Only an advert heard gives a path; a node known some other way has none, as in the firmware's table.
		p, err := s.st.Peers.GetByPubKey(ctx, cmd.PublicKey[:])
		if err != nil || p == nil || p.LastAdvertTS == 0 {
			c.send(errFrame(mc.ErrCodeNotFound))
			return
		}
		size := max(p.OutPathHashSize, 1)
		c.send(mc.AdvertPathResponse{RecvTimestamp: uint32(p.LastSeen.Unix()), PathLen: meshcore.MakePathLen(size, uint8(len(p.OutPath)/int(size))), Path: p.OutPath}.ToBytes())

	case mc.ExportContactCommand:
		if !cmd.Self {
			// Another node's advert as it was signed is not kept here, so it cannot be handed on.
			c.send(errFrame(mc.ErrCodeUnsupportedCmd))
			return
		}
		raw, err := comp.AppSelfAdvert()
		if err != nil {
			c.send(errFrame(mc.ErrCodeTableFull))
			return
		}
		c.send(mc.ExportContactResponse{AdvertData: raw}.ToBytes())

	case mc.ImportContactCommand:
		if !comp.AppImportAdvert(cmd.AdvertData) {
			c.send(errFrame(mc.ErrCodeIllegalArg))
			return
		}
		c.send(okFrame())

	case mc.SyncNextMessageCommand:
		var frame []byte
		ok, err := s.st.AppQueue.Waiting(ctx, s.id)
		if ok && err == nil {
			frame, ok, err = s.pop()
		}
		switch {
		case err != nil:
			c.send(errFrame(mc.ErrCodeFileIoError))
		case !ok:
			c.send(mc.NoMoreMessagesResponse{}.ToBytes())
		case c.session.appVer < 3 && (frame[0] == mc.RespContactMsgRecvV3 || frame[0] == mc.RespChannelMsgRecvV3):
			c.send(legacyMsgFrame(frame))
		default:
			c.send(frame)
		}

	case mc.SendTxtMsgCommand:
		c.sendText(comp, cmd)

	case mc.SendChannelTxtMsgCommand:
		// The firmware answers NOT_FOUND for an empty slot and a post it could not send alike.
		c.send(okOr(comp.SendAppChannelMessage(int(cmd.ChannelIdx), cmd.Text, c.session.scope), mc.ErrCodeNotFound))

	case mc.GetChannelCommand:
		if int(cmd.ChannelIdx) >= maxChannels {
			c.send(errFrame(mc.ErrCodeNotFound))
			return
		}
		// An empty slot reads blank, as in the firmware's fixed table; the app looks for one to add a channel in.
		info := mc.ChannelInfoResponse{ChannelIdx: cmd.ChannelIdx}
		if ch := comp.Node().Channel(int(cmd.ChannelIdx)); ch != nil {
			info.Name = ch.Name
			copy(info.Secret[:], ch.PSK)
		}
		c.send(info.ToBytes())

	case mc.SendSelfAdvertCommand:
		c.send(okOr(comp.SendAdvert(cmd.Flood), mc.ErrCodeTableFull))

	case mc.GetDeviceTimeCommand:
		c.send(mc.CurrTimeResponse{Timestamp: uint32(time.Now().Unix())}.ToBytes())

	case mc.SetDeviceTimeCommand:
		// The firmware takes a later time and refuses an earlier one; the host keeps its own clock, so a later one changes nothing here.
		if time.Unix(int64(cmd.EpochSecs), 0).Before(time.Now().Add(-clockSlack)) {
			c.send(errFrame(mc.ErrCodeIllegalArg))
			return
		}
		c.send(okFrame())

	case mc.GetBattAndStorageCommand:
		c.send(mc.BattAndStorageResponse{BatteryMilliVolts: batteryMV(env)}.ToBytes())

	case mc.GetStatsCommand:
		c.send(c.stats(comp, env, cmd.StatsType).ToBytes())

	case mc.GetTuningParamsCommand:
		c.send(tuning(env).ToBytes())

	case mc.GetCustomVarsCommand:
		c.send(mc.CustomVarsResponse{}.ToBytes())

	case mc.GetAutoAddConfigCommand:
		c.send(mc.AutoAddConfigResponse{}.ToBytes())

	case mc.GetAllowedRepeatFreqCommand:
		c.send(mc.AllowedRepeatFreqResponse{}.ToBytes())

	case mc.GetDefaultFloodScopeCommand:
		var scope mc.DefaultFloodScopeResponse
		if name, key, ok := companionScope(comp); ok {
			scope = mc.DefaultFloodScopeResponse{Name: name, Key: key[:]}
		}
		c.send(scope.ToBytes())

	case mc.SetFloodScopeCommand:
		switch {
		case cmd.Unscoped:
			c.session.scope = &companion.SendScope{Label: config.ScopeEverywhere}
		case cmd.TransportKey == nil || bytes.Equal(cmd.TransportKey, make([]byte, 16)): // the firmware's null key: no override
			c.session.scope = nil
		default:
			// Any key is taken, as by the firmware, and sent with as it is; one for a region not listed is stored as an unknown region.
			key := meshcore.RegionKey(cmd.TransportKey)
			label, ok := regionByKey(env.Regions, key[:])
			if !ok {
				label = config.ScopeUnknown
			}
			c.session.scope = &companion.SendScope{Label: label, Region: meshcore.NewRegionFromKey("", key)}
		}
		c.send(okFrame())

	case mc.SendLoginCommand:
		c.login(comp, cmd.PublicKey[:], cmd.Password)

	case mc.SendStatusReqCommand:
		var r [4]byte
		rand.Read(r[:])
		c.request(comp, cmd.PublicKey[:], meshcore.BuildRequest(meshcore.ReqTypeGetStatus, r), func(_ uint32, pub [32]byte, data []byte) []byte {
			return mc.PushStatusResp{PubKeyPrefix: prefix(pub), StatusData: data}.ToBytes()
		})

	case mc.SendTelemetryReqCommand:
		if cmd.Self {
			self := comp.Node().Identity().PublicKey()
			c.send(mc.PushTelemetryResp{PubKeyPrefix: prefix(self), LPPData: comp.AppSelfTelemetry(mc.MaxFrameSize - 8)}.ToBytes())
			return
		}
		var r [4]byte
		rand.Read(r[:])
		c.request(comp, cmd.PublicKey[:], meshcore.BuildRequest(meshcore.ReqTypeGetTelemetryData, r), func(_ uint32, pub [32]byte, data []byte) []byte {
			return mc.PushTelemetryResp{PubKeyPrefix: prefix(pub), LPPData: data}.ToBytes()
		})

	case mc.SendBinaryReqCommand:
		c.request(comp, cmd.PublicKey[:], cmd.RequestData, func(tag uint32, _ [32]byte, data []byte) []byte {
			return mc.PushBinaryResp{Tag: tag, ResponseData: data}.ToBytes()
		})

	case mc.SendAnonReqCommand:
		c.anonRequest(comp, cmd.PublicKey[:], cmd.RequestData)

	case mc.SendPathDiscoveryReqCommand:
		c.pathDiscovery(comp, cmd.PublicKey[:])

	case mc.SendTracePathCommand:
		c.trace(comp, cmd)

	case mc.HasConnectionCommand:
		if comp.Repeaters().Session(hex.EncodeToString(cmd.PublicKey[:])) == nil {
			c.send(errFrame(mc.ErrCodeNotFound))
			return
		}
		c.send(okFrame())

	case mc.LogoutCommand:
		comp.Repeaters().Logout(hex.EncodeToString(cmd.PublicKey[:]))
		c.send(okFrame())

	case mc.SendControlDataCommand:
		c.send(okOr(comp.AppSendControl(cmd.ControlData), mc.ErrCodeTableFull))

	case mc.SetPathHashModeCommand:
		size := int(cmd.Mode) + 1
		c.send(c.update(func(r *store.Companion) error {
			// An inherited size that already matches stays inherited.
			if (r.PathHashSize == nil && size == int(comp.PathHashSize())) || (r.PathHashSize != nil && *r.PathHashSize == size) {
				return errUnchanged
			}
			r.PathHashSize = &size
			return nil
		}, func(k *companion.Companion) bool { return int(k.PathHashSize()) == size }))

	case mc.SetAdvertNameCommand:
		name := firmwareName(cmd.Name)
		c.send(c.update(func(r *store.Companion) error {
			if r.Name == name {
				return errUnchanged
			}
			r.Name = name
			return nil
		}, func(k *companion.Companion) bool { return k.AppConfig().Name == name }))

	case mc.SetAdvertLatLonCommand:
		lat, lon := cmd.Latitude, cmd.Longitude
		c.send(c.update(func(r *store.Companion) error {
			if curLat, curLon := latLonE6(config.CompanionConfig{Latitude: r.Latitude, Longitude: r.Longitude}); lat == curLat && lon == curLon {
				return errUnchanged
			}
			r.Latitude, r.Longitude = nil, nil
			if lat != 0 || lon != 0 { // the firmware's 0,0 is no position
				la, lo := float64(lat)/1e6, float64(lon)/1e6
				r.Latitude, r.Longitude = &la, &lo
			}
			return nil
		}, func(k *companion.Companion) bool {
			curLat, curLon := latLonE6(k.AppConfig())
			return curLat == lat && curLon == lon
		}))

	case mc.SetOtherParamsCommand:
		c.send(c.otherParams(cmd))

	case mc.SetDefaultFloodScopeCommand:
		scope := config.ScopeEverywhere // a short frame clears the default: floods go out unscoped
		if cmd.Key != nil {
			sc, ok := regionByKey(env.Regions, cmd.Key)
			if !ok {
				c.send(errFrame(mc.ErrCodeIllegalArg))
				return
			}
			scope = sc
		}
		c.send(c.update(func(r *store.Companion) error {
			// An inherited scope that already resolves to this one stays inherited.
			if r.FloodScope == string(scope) || (r.FloodScope == string(config.ScopeInherit) && comp.AppConfig().FloodScope == scope) {
				return errUnchanged
			}
			r.FloodScope = string(scope)
			return nil
		}, func(k *companion.Companion) bool { return k.AppConfig().FloodScope == scope }))

	case mc.SetChannelCommand:
		switch err := comp.AppSetChannel(int(cmd.ChannelIdx), strings.ToValidUTF8(cmd.Name, ""), cmd.Secret[:]); {
		case errors.Is(err, companion.ErrNoSlot):
			c.send(errFrame(mc.ErrCodeNotFound))
		case err != nil:
			s.log.Info("app channel change refused", "slot", cmd.ChannelIdx, "name", cmd.Name, "error", err)
			c.send(errFrame(mc.ErrCodeIllegalArg))
		default:
			c.send(okOr(s.settings.SaveChannels(ctx, comp), mc.ErrCodeFileIoError))
		}

	case mc.ExportPrivateKeyCommand, mc.ImportPrivateKeyCommand:
		c.send(mc.DisabledResponse{}.ToBytes())

	case mc.SetRadioParamsCommand, mc.SetTxPowerCommand, mc.SetTuningParamsCommand, mc.SetAutoAddConfigCommand:
		// These belong to the radio and the host: a write that changes nothing is answered OK, so the app's save goes through, and a change is refused.
		if unchanged(env, cmd) {
			c.send(okFrame())
			return
		}
		c.send(errFrame(mc.ErrCodeUnsupportedCmd))

	default:
		c.send(errFrame(mc.ErrCodeUnsupportedCmd))
	}
}

func okOr(err error, code byte) []byte {
	if err != nil {
		return errFrame(code)
	}
	return okFrame()
}

func (c *conn) selfInfo(comp *companion.Companion, env *Env) mc.SelfInfoResponse {
	cfg := comp.AppConfig()
	si := mc.SelfInfoResponse{AdvertType: meshcore.AdvertTypeChat, TxPower: byte(env.Radio.TxPower), MaxTxPower: byte(env.Radio.TxPower),
		PublicKey:         comp.Node().Identity().PublicKey(),
		TelemetryModeBase: telemMode(cfg.TelemetryBase), TelemetryModeLoc: telemMode(cfg.TelemetryLocation), TelemetryModeEnv: telemMode(cfg.TelemetryEnvironment),
		ManualAddContacts: 1,
		RadioFrequency:    env.Radio.FreqHz / 1000, RadioBandwidth: env.Radio.BwHz, RadioSpreadFactor: env.Radio.SF, RadioCodingRate: env.Radio.CR,
		Name: cfg.Name,
	}
	if cfg.SharesLocation() {
		si.AdvertLocPolicy = 1
	}
	si.AdvertLatitude, si.AdvertLongitude = latLonE6(cfg)
	return si
}

func latLonE6(cfg config.CompanionConfig) (int32, int32) {
	if !cfg.HasLatLon() {
		return 0, 0
	}
	return int32(math.Round(*cfg.Latitude * 1e6)), int32(math.Round(*cfg.Longitude * 1e6))
}

// telemMode is the firmware's TELEM_MODE_* for a telemetry policy: deny 0, the contact's own flags 1, everyone 2.
func telemMode(m *string) byte {
	switch config.TelemetryModeOrDefault(m) {
	case config.TelemetrySelected:
		return 1
	case config.TelemetryContacts:
		return 2
	}
	return 0
}

// companionScope is the region the companion's own floods go in, as the app's default scope.
func companionScope(comp *companion.Companion) (string, meshcore.RegionKey, bool) {
	name, ok := comp.AppConfig().FloodScope.RegionName()
	if !ok {
		return "", meshcore.RegionKey{}, false
	}
	return name, meshcore.NewRegion(name).Key, true
}

// regionByKey names a scope key the app chose; a key for a region OwlShack does not know cannot be sent with.
func regionByKey(regions []string, key []byte) (config.FloodScope, bool) {
	for _, r := range regions {
		if k := meshcore.NewRegion(r).Key; bytes.Equal(k[:], key) {
			return config.FloodScope("region:" + r), true
		}
	}
	return "", false
}

func (s *Server) contact(pub []byte) (*store.Contact, bool) {
	k, err := s.st.Contacts.Get(context.Background(), s.id, pub)
	return k, err == nil && k != nil
}

// contactByPrefix is the firmware's lookupContactByPubKey on the 6-byte prefix a text send carries.
func (s *Server) contactByPrefix(prefix []byte) (*store.Contact, bool) {
	contacts, err := s.st.Contacts.List(context.Background(), s.id)
	if err != nil {
		return nil, false
	}
	for i := range contacts {
		if bytes.HasPrefix(contacts[i].PeerPubKey, prefix) {
			return &contacts[i], true
		}
	}
	return nil, false
}

func (c *conn) sendText(comp *companion.Companion, cmd mc.SendTxtMsgCommand) {
	s := c.srv
	k, ok := s.contactByPrefix(cmd.PubKeyPrefix[:])
	if !ok {
		c.send(errFrame(mc.ErrCodeNotFound))
		return
	}
	pubHex := hex.EncodeToString(k.PeerPubKey)
	var pub [32]byte
	copy(pub[:], k.PeerPubKey)

	switch cmd.TxtType {
	case meshcore.TxtTypePlain:
		res, err := comp.SendAppDM(pubHex, cmd.Text, cmd.SenderTimestamp, int(cmd.Attempt), c.session.scope, func(ack uint32, rt time.Duration) {
			s.push(mc.PushSendConfirmedResponse{AckCode: ack, RoundTrip: uint32(rt.Milliseconds())}.ToBytes())
		})
		if errors.Is(err, node.ErrTextTooLong) {
			c.send(errFrame(mc.ErrCodeIllegalArg))
			return
		}
		if err != nil {
			c.send(errFrame(mc.ErrCodeTableFull))
			return
		}
		c.send(sentFrame(res.Flood, res.ACK, uint32(res.Timeout.Milliseconds())))

	case meshcore.TxtTypeCLIData, meshcore.TxtTypeCLICommand:
		// Sent as the firmware's sendCommandData: no login of ours needed, and a CLI reply, not an ACK, answers it, which reaches the app as a message whenever it comes.
		c.async(func(sent func(uint32, time.Duration, bool)) error {
			return comp.Repeaters().AppCLI(pubHex, cmd.Text, cmd.TxtType, cmd.Attempt, c.appSend(comp, pub[:], sent))
		})

	default:
		c.send(errFrame(mc.ErrCodeUnsupportedCmd))
	}
}

func (c *conn) login(comp *companion.Companion, pubBytes []byte, password string) {
	s := c.srv
	k, ok := s.contact(pubBytes)
	if !ok {
		c.send(errFrame(mc.ErrCodeNotFound))
		return
	}
	var pub [32]byte
	copy(pub[:], pubBytes)
	pubHex := hex.EncodeToString(pubBytes)
	var since *uint32
	if k.Type == "ROOM" {
		// The firmware keeps a room's sync_since from the posts it got; ours are the posts stored.
		v := uint32(0)
		if m, err := s.st.Messages.LatestRx(context.Background(), s.id, "dm:"+pubHex); err == nil && m != nil {
			v = uint32(m.Timestamp.Unix())
		}
		since = &v
	}
	c.async(func(sent func(uint32, time.Duration, bool)) error {
		res, err := comp.Repeaters().AppLogin(pubHex, password, since, reqTimeout, c.appSend(comp, pubBytes, sent))
		if err == nil {
			s.push(loginFrame(pub, res.Reply))
		}
		return err
	})
}

// request sends a contact request for the app and pushes what answers it; nothing is pushed for a request that goes unanswered, as on the firmware.
func (c *conn) request(comp *companion.Companion, pubBytes, body []byte, push func(tag uint32, pub [32]byte, data []byte) []byte) {
	s := c.srv
	if _, ok := s.contact(pubBytes); !ok {
		c.send(errFrame(mc.ErrCodeNotFound))
		return
	}
	var pub [32]byte
	copy(pub[:], pubBytes)
	c.async(func(sent func(uint32, time.Duration, bool)) error {
		var tag uint32
		data, err := comp.Repeaters().AppRequest(hex.EncodeToString(pubBytes), body, reqTimeout, c.appSend(comp, pubBytes, func(t uint32, w time.Duration, f bool) {
			tag = t
			sent(t, w, f)
		}))
		if err == nil {
			s.push(push(tag, pub, data))
		}
		return err
	})
}

func (c *conn) anonRequest(comp *companion.Companion, pubBytes, body []byte) {
	s := c.srv
	c.async(func(sent func(uint32, time.Duration, bool)) error {
		var tag uint32
		data, err := comp.Repeaters().AppAnonRequest(hex.EncodeToString(pubBytes), body, reqTimeout, c.appSend(comp, pubBytes, func(t uint32, w time.Duration, f bool) {
			tag = t
			sent(t, w, f)
		}))
		if err == nil {
			s.push(mc.PushBinaryResp{Tag: tag, ResponseData: data}.ToBytes())
		}
		return err
	})
}

func (c *conn) pathDiscovery(comp *companion.Companion, pubBytes []byte) {
	s := c.srv
	if _, ok := s.contact(pubBytes); !ok {
		c.send(errFrame(mc.ErrCodeNotFound))
		return
	}
	var pub [32]byte
	copy(pub[:], pubBytes)
	c.async(func(sent func(uint32, time.Duration, bool)) error {
		out, in, err := comp.Repeaters().AppPathDiscovery(hex.EncodeToString(pubBytes), reqTimeout, c.appSend(comp, pubBytes, sent))
		if err == nil {
			s.push(mc.PushPathDiscoveryResp{PubKeyPrefix: prefix(pub), OutPathLen: out.Len, OutPath: out.Hops, InPathLen: in.Len, InPath: in.Hops}.ToBytes())
		}
		return err
	})
}

// async runs a request whose RESP_CODE_SENT comes once it is on its way and whose answer is pushed later; one that never went out is refused.
func (c *conn) async(run func(sent func(tag uint32, wait time.Duration, flood bool)) error) {
	sent := make(chan struct{})
	go func() {
		err := run(func(tag uint32, wait time.Duration, flood bool) {
			c.send(sentFrame(flood, tag, uint32(wait.Milliseconds())))
			close(sent)
		})
		select {
		case <-sent:
		default:
			c.srv.log.Info("app request not sent", "error", err)
			c.send(errFrame(sendErrCode(err)))
			close(sent)
		}
	}()
	<-sent
}

func sendErrCode(err error) byte {
	switch {
	case errors.Is(err, repeater.ErrUnknownPeer), errors.Is(err, repeater.ErrBadPubkey):
		return mc.ErrCodeNotFound
	}
	return mc.ErrCodeTableFull
}

func (c *conn) trace(comp *companion.Companion, cmd mc.SendTracePathCommand) {
	sz := cmd.Flags & 0x03
	if err := comp.AppSendTrace(cmd.Tag, cmd.Auth, cmd.Path, 1<<sz); err != nil {
		c.send(errFrame(mc.ErrCodeTableFull))
		return
	}
	env := c.srv.env.Load()
	airtime := env.Stats.EstAirtimeMs(len(cmd.Path) + 11 + 2)
	wait := node.CalcDirectTimeout(airtime, uint8(len(cmd.Path)>>sz))
	c.send(sentFrame(false, cmd.Tag, uint32(max(wait, reqTimeout).Milliseconds())))
}

func batteryMV(env *Env) uint16 {
	if ds := env.Stats.CachedStats(); ds.HaveBattery {
		return ds.BatteryMV
	}
	return 0
}

func tuning(env *Env) mc.TuningParamsResponse {
	return mc.TuningParamsResponse{AirtimeFactor: float32(env.AirtimeFactor)}
}

func (c *conn) stats(comp *companion.Companion, env *Env, kind byte) mc.StatsResponse {
	s := c.srv
	switch kind {
	case mc.StatsTypeCore:
		return mc.StatsResponse{StatsType: kind, Core: &mc.CoreStats{BatteryMV: batteryMV(env), UptimeSecs: uint32(time.Since(env.Started).Seconds()),
			QueueLen: byte(min(comp.Node().TxQueueLen(), 255))}}
	case mc.StatsTypeRadio:
		var floor int16
		if ds := env.Stats.CachedStats(); ds.HaveNoiseFloor {
			floor = ds.NoiseFloor
		}
		return mc.StatsResponse{StatsType: kind, Radio: &mc.RadioStats{NoiseFloor: floor, LastRSSI: int8(s.rx.lastRSSI.Load()), LastSNR: math.Float32frombits(s.rx.lastSNR.Load()),
			TxAirSecs: uint32(env.Tx.TxStats().AirtimeMs / 1000), RxAirSecs: uint32(s.rx.airMs.Load() / 1000)}}
	}
	tx := env.Tx.TxStats()
	var errs uint64
	if e := env.Stats.LinkStats().RecvErrors; e != nil {
		errs = *e
	}
	return mc.StatsResponse{StatsType: kind, Packets: &mc.PacketStats{PacketsRecv: uint32(s.rx.flood.Load() + s.rx.direct.Load()),
		PacketsSent: uint32(tx.Sent), SentFlood: uint32(tx.SentFlood), SentDirect: uint32(tx.SentDirect),
		RecvFlood: uint32(s.rx.flood.Load()), RecvDirect: uint32(s.rx.direct.Load()), RecvErrors: uint32(errs)}}
}

func (c *conn) addUpdateContact(comp *companion.Companion, cmd mc.AddUpdateContactCommand) []byte {
	s := c.srv
	ctx := context.Background()
	pub, advType, flags, outLen, name := cmd.PublicKey[:], cmd.Type, cmd.Flags, cmd.OutPathLen, cmd.Name
	if size, hops := meshcore.PathLenFields(outLen); outLen != mc.OutPathUnknown && (size < config.MinPathHashSize || size > config.MaxPathHashSize || int(size)*int(hops) > len(cmd.OutPath)) {
		return errFrame(mc.ErrCodeIllegalArg)
	}
	self := comp.Node().Identity().PublicKey()
	if bytes.Equal(pub, self[:]) {
		return errFrame(mc.ErrCodeIllegalArg)
	}
	id, err := meshcore.NewIdentityFromBytes(pub)
	if err != nil {
		return errFrame(mc.ErrCodeIllegalArg)
	}
	typeName := advNames[advType]

	existing, isContact := s.contact(pub)
	if isContact && existing.Type != typeName {
		// A contact's type is what its adverts say it is.
		return errFrame(mc.ErrCodeUnsupportedCmd)
	}
	if name == "" {
		return errFrame(mc.ErrCodeIllegalArg)
	}
	lastAdvert := cmd.LastAdvert
	var werr error
	s.st.WriteSync(func() {
		if !isContact {
			p, _ := s.st.Peers.GetByPubKey(ctx, pub)
			if p == nil {
				if werr = s.st.Peers.Upsert(ctx, &store.Peer{PubKey: pub, Name: name, Type: typeName, LastSeen: time.Now()}); werr != nil {
					return
				}
			}
			if werr = s.st.Contacts.Add(ctx, s.id, pub, name, typeName); werr != nil {
				return
			}
			// A node heard keeps the advert heard, as the REST add does; one never heard takes the frame's, as the firmware does.
			if p != nil && p.LastAdvertTS != 0 {
				werr = s.st.Contacts.SetLastAdvert(ctx, s.id, pub, p.LastSeen, p.LastAdvertTS)
			} else if lastAdvert != 0 {
				werr = s.st.Contacts.SetLastAdvert(ctx, s.id, pub, time.Time{}, lastAdvert)
			}
			if werr != nil {
				return
			}
		}
		if isContact && existing.Name != name {
			if werr = s.st.Contacts.SetName(ctx, s.id, pub, name); werr != nil {
				return
			}
		}
		if cmd.HasLatLon && (!isContact || existing.Lat != cmd.Latitude || existing.Lon != cmd.Longitude) {
			if werr = s.st.Contacts.SetLocation(ctx, s.id, pub, cmd.Latitude, cmd.Longitude); werr != nil {
				return
			}
		}
		k, err := s.st.Contacts.Get(ctx, s.id, pub)
		if err != nil {
			werr = err
			return
		}
		// Bits above the three telemetry classes, such as the firmware's remote CLI flag, are not kept.
		if perms, fav := flags>>1&0x07, flags&1 == 1; k.Metadata.TelemPerms != perms || k.Metadata.Favourite != fav {
			k.Metadata.TelemPerms, k.Metadata.Favourite = perms, fav
			werr = s.st.Contacts.UpdateMetadata(ctx, s.id, pub, k.Metadata)
		}
	})
	if werr != nil {
		return errFrame(mc.ErrCodeFileIoError)
	}
	if comp.Node().Peers().Lookup(id.PublicKey()) == nil {
		comp.Node().Peers().Insert(&node.Peer{Identity: id, Name: name, Type: typeName, LastSeen: time.Now()})
	}
	pubHex := hex.EncodeToString(pub)
	if outLen == mc.OutPathUnknown {
		if isContact && existing.OutPath != nil {
			comp.Repeaters().ResetPeerPath(pubHex)
		}
		return okFrame()
	}
	size, _ := meshcore.PathLenFields(outLen)
	path := cmd.OutPath
	if isContact && existing.OutPath != nil && bytes.Equal(existing.OutPath, path) && existing.OutPathHashSize == size {
		return okFrame()
	}
	return okOr(comp.Repeaters().SetPeerPath(pubHex, path, size), mc.ErrCodeIllegalArg)
}

// unchanged reports whether a settings write leaves everything as it is.
func unchanged(env *Env, cmd any) bool {
	switch cmd := cmd.(type) {
	case mc.SetRadioParamsCommand:
		return cmd == mc.SetRadioParamsCommand{Frequency: env.Radio.FreqHz / 1000, Bandwidth: env.Radio.BwHz, SpreadFactor: env.Radio.SF, CodingRate: env.Radio.CR}
	case mc.SetTxPowerCommand:
		return int8(cmd.TxPower) == int8(env.Radio.TxPower)
	case mc.SetTuningParamsCommand:
		// Read back as the app reads what GET_TUNING_PARAMS sent, so an unchanged write matches it exactly.
		want, err := mc.ParseSetTuningParamsCommand(append([]byte{mc.CmdSetTuningParams}, tuning(env).ToBytes()[1:]...))
		return err == nil && cmd == want
	case mc.SetAutoAddConfigCommand:
		return cmd.Config == 0 && cmd.MaxHops == 0
	}
	return false
}

// errUnchanged ends a change that would write what is already saved: no write, no reload.
var errUnchanged = errors.New("unchanged")

// liveWait bounds how long an OK waits for the reload that puts a saved setting into the running companion.
const liveWait = 5 * time.Second

// update saves a change to the companion's settings, checked against the saved row, and answers OK once live says the running companion has it.
func (c *conn) update(change func(*store.Companion) error, live func(*companion.Companion) bool) []byte {
	err := c.srv.settings.UpdateCompanion(context.Background(), c.srv.id, change)
	if err != nil && !errors.Is(err, errUnchanged) {
		c.srv.log.Info("app settings change refused", "error", err)
		return errFrame(mc.ErrCodeIllegalArg)
	}
	for end := time.Now().Add(liveWait); !live(c.srv.comp.Load()); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			c.srv.log.Warn("app settings change saved but not yet running", "after", liveWait)
			break
		}
	}
	return okFrame()
}

// firmwareName is the name as the firmware keeps it: 31 bytes at most, here never cutting a character in two.
func firmwareName(name string) string {
	return strings.ToValidUTF8(meshcore.TruncateUTF8(name, 31), "")
}

// telemModes are the firmware's TELEM_MODE_* by value: deny, the contact's own flags, everyone.
var telemModes = []string{config.TelemetryDeny, config.TelemetrySelected, config.TelemetryContacts}

// otherParams is SET_OTHER_PARAMS: the telemetry modes and sharing the position are the companion's; adding contacts from adverts and extra acks are not done here.
func (c *conn) otherParams(cmd mc.SetOtherParamsCommand) []byte {
	if cmd.ManualAddContacts != 1 || (cmd.HasExtended && cmd.MultiAcks != 0) {
		return errFrame(mc.ErrCodeUnsupportedCmd)
	}
	var modes []string
	if cmd.HasTelemetryModes {
		for _, v := range []byte{cmd.TelemetryModeBase, cmd.TelemetryModeLoc, cmd.TelemetryModeEnv} {
			if int(v) >= len(telemModes) {
				return errFrame(mc.ErrCodeIllegalArg)
			}
			modes = append(modes, telemModes[v])
		}
	}
	share := cmd.HasAdvertLocPolicy && cmd.AdvertLocPolicy != 0 // the firmware shares for any policy but none
	live := func(k *companion.Companion) bool {
		cfg := k.AppConfig()
		got := []string{config.TelemetryModeOrDefault(cfg.TelemetryBase), config.TelemetryModeOrDefault(cfg.TelemetryLocation), config.TelemetryModeOrDefault(cfg.TelemetryEnvironment)}
		return (!cmd.HasAdvertLocPolicy || cfg.SharesLocation() == share) && (modes == nil || slices.Equal(got, modes))
	}
	return c.update(func(r *store.Companion) error {
		same := !cmd.HasAdvertLocPolicy || share == r.ShareLocation
		if modes != nil {
			same = same && modes[0] == r.TelemBase && modes[1] == r.TelemLoc && modes[2] == r.TelemEnv
		}
		if same {
			return errUnchanged
		}
		if modes != nil {
			r.TelemBase, r.TelemLoc, r.TelemEnv = modes[0], modes[1], modes[2]
		}
		if cmd.HasAdvertLocPolicy {
			r.ShareLocation = share
		}
		return nil
	}, live)
}
