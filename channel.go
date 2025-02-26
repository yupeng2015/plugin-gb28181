package gb28181

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sync/atomic"

	"github.com/ghettovoice/gosip/sip"
	"github.com/goccy/go-json"
	"go.uber.org/zap"
	. "m7s.live/engine/v4"
	"m7s.live/engine/v4/log"
	"m7s.live/plugin/gb28181/v4/utils"
	"m7s.live/plugin/ps/v4"
)

var QUERY_RECORD_TIMEOUT = time.Second * 5

type PullStream struct {
	opt       *InviteOptions
	channel   *Channel
	inviteRes sip.Response
}

func (p *PullStream) CreateRequest(method sip.RequestMethod) (req sip.Request) {
	res := p.inviteRes
	req = p.channel.CreateRequst(method)
	from, _ := res.From()
	to, _ := res.To()

	callId, _ := res.CallID()
	req.ReplaceHeaders(from.Name(), []sip.Header{from})
	req.ReplaceHeaders(to.Name(), []sip.Header{to})
	req.ReplaceHeaders(callId.Name(), []sip.Header{callId})
	return
}

func (p *PullStream) Bye() int {
	req := p.CreateRequest(sip.BYE)
	resp, err := p.channel.Device.SipRequestForResponse(req)
	if p.opt.IsLive() {
		p.channel.State.Store(0)
	}
	if p.opt.recyclePort != nil {
		p.opt.recyclePort(p.opt.MediaPort)
	}
	if err != nil {
		return http.StatusInternalServerError
	}
	return int(resp.StatusCode())
}

func (p *PullStream) info(body string) int {
	d := p.channel.Device
	req := p.CreateRequest(sip.INFO)
	contentType := sip.ContentType("Application/MANSRTSP")
	req.AppendHeader(&contentType)
	req.SetBody(body, true)

	resp, err := d.SipRequestForResponse(req)
	if err != nil {
		log.Warnf("Send info to stream error: %v, stream=%s, body=%s", err, p.opt.StreamPath, body)
		return getSipRespErrorCode(err)
	}
	return int(resp.StatusCode())
}

// 暂停播放
func (p *PullStream) Pause() int {
	body := fmt.Sprintf(`PAUSE RTSP/1.0
CSeq: %d
PauseTime: now
`, p.channel.Device.SN)
	return p.info(body)
}

// 恢复播放
func (p *PullStream) Resume() int {
	d := p.channel.Device
	body := fmt.Sprintf(`PLAY RTSP/1.0
CSeq: %d
Range: npt=now-
`, d.SN)
	return p.info(body)
}

// 跳转到播放时间
// second: 相对于起始点调整到第 sec 秒播放
func (p *PullStream) PlayAt(second uint) int {
	d := p.channel.Device
	body := fmt.Sprintf(`PLAY RTSP/1.0
CSeq: %d
Range: npt=%d-
`, d.SN, second)
	return p.info(body)
}

// 快进/快退播放
// speed 取值： 0.25 0.5 1 2 4 或者其对应的负数表示倒放
func (p *PullStream) PlayForward(speed float32) int {
	d := p.channel.Device
	body := fmt.Sprintf(`PLAY RTSP/1.0
CSeq: %d
Scale: %0.6f
`, d.SN, speed)
	return p.info(body)
}

type Channel struct {
	Device      *Device      `json:"-" yaml:"-"` // 所属设备
	State       atomic.Int32 `json:"-" yaml:"-"` // 通道状态,0:空闲,1:正在invite,2:正在播放/对讲
	LiveSubSP   string       // 实时子码流，通过rtsp
	GpsTime     time.Time    // gps时间
	Longitude   string       // 经度
	Latitude    string       // 纬度
	*log.Logger `json:"-" yaml:"-"`
	ChannelInfo
}

type PresetInfo struct {
	PresetID   int    `json:"-" yaml:"-"` //
	PresetName string `json:"-" yaml:"-"` //
}

func (c *Channel) MarshalJSON() ([]byte, error) {
	m := map[string]any{
		"DeviceID":     c.DeviceID,
		"ParentID":     c.ParentID,
		"Name":         c.Name,
		"Manufacturer": c.Manufacturer,
		"Model":        c.Model,
		"Owner":        c.Owner,
		"CivilCode":    c.CivilCode,
		"Address":      c.Address,
		"Port":         c.Port,
		"Parental":     c.Parental,
		"SafetyWay":    c.SafetyWay,
		"RegisterWay":  c.RegisterWay,
		"Secrecy":      c.Secrecy,
		"Status":       c.Status,
		"Longitude":    c.Longitude,
		"Latitude":     c.Latitude,
		"GpsTime":      c.GpsTime,
		"LiveSubSP":    c.LiveSubSP,
		"LiveStatus":   c.State.Load(),
	}
	return json.Marshal(m)
}

// Channel 通道
type ChannelInfo struct {
	DeviceID     string // 通道ID
	ParentID     string
	Name         string
	Manufacturer string
	Model        string
	Owner        string
	CivilCode    string
	Address      string
	Port         int
	Parental     int
	SafetyWay    int
	RegisterWay  int
	Secrecy      int
	Status       ChannelStatus
}

type ChannelStatus string

const (
	ChannelOnStatus  = "ON"
	ChannelOffStatus = "OFF"
)

func (channel *Channel) CreateRequst(Method sip.RequestMethod) (req sip.Request) {
	d := channel.Device
	d.SN++

	callId := sip.CallID(utils.RandNumString(10))
	userAgent := sip.UserAgentHeader("Monibuca")
	maxForwards := sip.MaxForwards(70) //增加max-forwards为默认值 70
	cseq := sip.CSeq{
		SeqNo:      uint32(d.SN),
		MethodName: Method,
	}
	port := sip.Port(conf.SipPort)
	serverAddr := sip.Address{
		//DisplayName: sip.String{Str: d.serverConfig.Serial},
		Uri: &sip.SipUri{
			FUser: sip.String{Str: conf.Serial},
			FHost: d.SipIP,
			FPort: &port,
		},
		Params: sip.NewParams().Add("tag", sip.String{Str: utils.RandNumString(9)}),
	}
	//非同一域的目标地址需要使用@host
	host := conf.Realm
	if channel.DeviceID[0:10] != host {
		if channel.Port != 0 {
			deviceIp := d.NetAddr
			deviceIp = deviceIp[0:strings.LastIndex(deviceIp, ":")]
			host = fmt.Sprintf("%s:%d", deviceIp, channel.Port)
		} else {
			host = d.NetAddr
		}
	}

	channelAddr := sip.Address{
		//DisplayName: sip.String{Str: d.serverConfig.Serial},
		Uri: &sip.SipUri{FUser: sip.String{Str: channel.DeviceID}, FHost: host},
	}
	req = sip.NewRequest(
		"",
		Method,
		channelAddr.Uri,
		"SIP/2.0",
		[]sip.Header{
			serverAddr.AsFromHeader(),
			channelAddr.AsToHeader(),
			&callId,
			&userAgent,
			&cseq,
			&maxForwards,
			serverAddr.AsContactHeader(),
		},
		"",
		nil,
	)

	req.SetTransport(conf.SipNetwork)
	req.SetDestination(d.NetAddr)
	return req
}

func (channel *Channel) QueryRecord(startTime, endTime string) ([]*Record, error) {
	d := channel.Device
	request := d.CreateRequest(sip.MESSAGE)
	contentType := sip.ContentType("Application/MANSCDP+xml")
	request.AppendHeader(&contentType)
	// body := fmt.Sprintf(`<?xml version="1.0"?>
	// 	<Query>
	// 	<CmdType>RecordInfo</CmdType>
	// 	<SN>%d</SN>
	// 	<DeviceID>%s</DeviceID>
	// 	<StartTime>%s</StartTime>
	// 	<EndTime>%s</EndTime>
	// 	<Secrecy>0</Secrecy>
	// 	<Type>all</Type>
	// 	</Query>`, d.sn, channel.DeviceID, startTime, endTime)
	start, _ := strconv.ParseInt(startTime, 10, 0)
	end, _ := strconv.ParseInt(endTime, 10, 0)
	body := BuildRecordInfoXML(d.SN, channel.DeviceID, start, end)
	request.SetBody(body, true)

	resultCh := RecordQueryLink.WaitResult(d.ID, channel.DeviceID, d.SN, QUERY_RECORD_TIMEOUT)
	resp, err := d.SipRequestForResponse(request)
	if err != nil {
		return nil, fmt.Errorf("query error: %s", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("query error, status=%d", resp.StatusCode())
	}
	// RecordQueryLink 中加了超时机制，该结果一定会返回
	// 所以此处不用再增加超时等保护机制
	r := <-resultCh
	return r.list, r.err
}

func (channel *Channel) QueryPresetList() (sip.Response, error) {
	d := channel.Device
	request := d.CreateRequest(sip.MESSAGE)
	contentType := sip.ContentType("Application/MANSCDP+xml")
	request.AppendHeader(&contentType)

	body := BuildPresetListXML(100, channel.DeviceID)
	request.SetBody(body, true)

	resp, err := d.SipRequestForResponse(request)
	if err != nil {
		return nil, fmt.Errorf("query error: %s", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("query error, status=%d", resp.StatusCode())
	}
	return resp, nil
}

func (channel *Channel) PresetControl(ptzCode int, point byte) int {
	cmd := byte(PresetSet)
	switch ptzCode {
	case PresetAddPoint:
		cmd = PresetSet
	case PresetDelPoint:
		cmd = PresetDel
	case PresetCallPoint:
		cmd = PresetCall
	default:

	}
	PTZCmd := Pack(cmd, point)
	return channel.Control(PTZCmd)
}

func (channel *Channel) Control(PTZCmd string) int {
	d := channel.Device
	request := d.CreateRequest(sip.MESSAGE)
	contentType := sip.ContentType("Application/MANSCDP+xml")
	request.AppendHeader(&contentType)
	body := fmt.Sprintf(`<?xml version="1.0"?>
		<Control>
		<CmdType>DeviceControl</CmdType>
		<SN>%d</SN>
		<DeviceID>%s</DeviceID>
		<PTZCmd>%s</PTZCmd>
		</Control>`, d.SN, channel.DeviceID, PTZCmd)
	request.SetBody(body, true)
	resp, err := d.SipRequestForResponse(request)
	if err != nil {
		return http.StatusRequestTimeout
	}
	return int(resp.StatusCode())
}

// Invite 发送Invite报文 invites a channel to play
// 注意里面的锁保证不同时发送invite报文，该锁由channel持有
/***
f字段： f = v/编码格式/分辨率/帧率/码率类型/码率大小a/编码格式/码率大小/采样率
各项具体含义：
    v：后续参数为视频的参数；各参数间以 “/”分割；
编码格式：十进制整数字符串表示
1 –MPEG-4 2 –H.264 3 – SVAC 4 –3GP
    分辨率：十进制整数字符串表示
1 – QCIF 2 – CIF 3 – 4CIF 4 – D1 5 –720P 6 –1080P/I
帧率：十进制整数字符串表示 0～99
码率类型：十进制整数字符串表示
1 – 固定码率（CBR）     2 – 可变码率（VBR）
码率大小：十进制整数字符串表示 0～100000（如 1表示1kbps）
    a：后续参数为音频的参数；各参数间以 “/”分割；
编码格式：十进制整数字符串表示
1 – G.711    2 – G.723.1     3 – G.729      4 – G.722.1
码率大小：十进制整数字符串
音频编码码率： 1 — 5.3 kbps （注：G.723.1中使用）
   2 — 6.3 kbps （注：G.723.1中使用）
   3 — 8 kbps （注：G.729中使用）
   4 — 16 kbps （注：G.722.1中使用）
   5 — 24 kbps （注：G.722.1中使用）
   6 — 32 kbps （注：G.722.1中使用）
   7 — 48 kbps （注：G.722.1中使用）
   8 — 64 kbps（注：G.711中使用）
采样率：十进制整数字符串表示
	1 — 8 kHz（注：G.711/ G.723.1/ G.729中使用）
	2—14 kHz（注：G.722.1中使用）
	3—16 kHz（注：G.722.1中使用）
	4—32 kHz（注：G.722.1中使用）
	注1：字符串说明
本节中使用的“十进制整数字符串”的含义为“0”～“4294967296” 之间的十进制数字字符串。
注2：参数分割标识
各参数间以“/”分割，参数间的分割符“/”不能省略；
若两个分割符 “/”间的某参数为空时（即两个分割符 “/”直接将相连时）表示无该参数值；
注3：f字段说明
使用f字段时，应保证视频和音频参数的结构完整性，即在任何时候，f字段的结构都应是完整的结构：
f = v/编码格式/分辨率/帧率/码率类型/码率大小a/编码格式/码率大小/采样率
若只有视频时，音频中的各参数项可以不填写，但应保持 “a///”的结构:
f = v/编码格式/分辨率/帧率/码率类型/码率大小a///
若只有音频时也类似处理，视频中的各参数项可以不填写，但应保持 “v/”的结构：
f = v/a/编码格式/码率大小/采样率
f字段中视、音频参数段之间不需空格分割。
可使用f字段中的分辨率参数标识同一设备不同分辨率的码流。
*/

func (channel *Channel) Invite(opt *InviteOptions) (code int, err error) {
	if opt.IsLive() {
		if !channel.State.CompareAndSwap(0, 1) {
			return 304, nil
		}
		defer func() {
			if err != nil {
				GB28181Plugin.Error("InviteRetryInit", zap.Error(err))
				channel.State.Store(0)
				if conf.InviteMode == 1 {
					// 5秒后重试
					time.AfterFunc(time.Second*5, func() {
						channel.Invite(opt)
					})
				}
			} else {
				channel.State.Store(2)
			}
		}()
	}

	d := channel.Device
	streamPath := fmt.Sprintf("%s/%s", d.ID, channel.DeviceID)
	s := "Play"
	opt.CreateSSRC()
	if opt.Record() {
		s = "Playback"
		streamPath = fmt.Sprintf("%s/%s/%d-%d", d.ID, channel.DeviceID, opt.Start, opt.End)
	}
	if opt.StreamPath != "" {
		streamPath = opt.StreamPath
	} else if channel.DeviceID == "" {
		streamPath = "gb28181/" + d.ID
	} else {
		opt.StreamPath = streamPath
	}
	if opt.dump == "" {
		opt.dump = conf.DumpPath
	}
	protocol := ""
	networkType := "udp"

	// 根据配置文件判断是否多路复用
	reusePort := conf.Port.Fdm

	if conf.IsMediaNetworkTCP() {
		networkType = "tcp"
		protocol = "TCP/"
		if conf.tcpPorts.Valid {
			opt.MediaPort, err = conf.tcpPorts.GetPort()
			opt.recyclePort = conf.tcpPorts.Recycle
		}
	} else {
		if conf.udpPorts.Valid {
			opt.MediaPort, err = conf.udpPorts.GetPort()
			opt.recyclePort = conf.udpPorts.Recycle
		}
	}
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if opt.MediaPort == 0 {
		opt.MediaPort = conf.MediaPort
		// 单端口默认多路复用
		reusePort = true
	}

	sdpInfo := []string{
		"v=0",
		fmt.Sprintf("o=%s 0 0 IN IP4 %s", channel.DeviceID, d.MediaIP),
		"s=" + s,
		"u=" + channel.DeviceID + ":0",
		"c=IN IP4 " + d.MediaIP,
		opt.String(),
		fmt.Sprintf("m=video %d %sRTP/AVP 96", opt.MediaPort, protocol),
		"a=recvonly",
		"a=rtpmap:96 PS/90000",
	}
	if conf.IsMediaNetworkTCP() {
		sdpInfo = append(sdpInfo, "a=setup:passive", "a=connection:new")
	}
	sdpInfo = append(sdpInfo, "y="+opt.ssrc)
	invite := channel.CreateRequst(sip.INVITE)
	contentType := sip.ContentType("application/sdp")
	invite.AppendHeader(&contentType)

	invite.SetBody(strings.Join(sdpInfo, "\r\n")+"\r\n", true)

	subject := sip.GenericHeader{
		HeaderName: "Subject", Contents: fmt.Sprintf("%s:%s,%s:0", channel.DeviceID, opt.ssrc, conf.Serial),
	}
	invite.AppendHeader(&subject)
	inviteRes, err := d.SipRequestForResponse(invite)
	if err != nil {
		if opt.recyclePort != nil {
			opt.recyclePort(opt.MediaPort)
		}
		channel.Error("inviteRequestError", zap.Error(err), zap.String("msg", invite.String()))
		return http.StatusInternalServerError, err
	}
	code = int(inviteRes.StatusCode())
	channel.Info("invite response", zap.Int("status code", code))

	if code == http.StatusOK {
		ds := strings.Split(inviteRes.Body(), "\r\n")
		for _, l := range ds {
			if ls := strings.Split(l, "="); len(ls) > 1 {
				if ls[0] == "y" && len(ls[1]) > 0 {
					if _ssrc, err := strconv.ParseInt(ls[1], 10, 0); err == nil {
						opt.SSRC = uint32(_ssrc)
					} else {
						channel.Error("read invite response y ", zap.Error(err))
					}
					//	break
				}
				if ls[0] == "m" && len(ls[1]) > 0 {
					netinfo := strings.Split(ls[1], " ")
					if strings.ToUpper(netinfo[2]) == "TCP/RTP/AVP" {
						channel.Debug("Device support tcp")
					} else {
						channel.Debug("Device not support tcp")
						networkType = "udp"
					}
				}
			}
		}
		var psPuber ps.PSPublisher
		err = psPuber.Receive(streamPath, opt.dump, fmt.Sprintf("%s:%d", networkType, opt.MediaPort), opt.SSRC, reusePort)
		if err != nil {
			if opt.recyclePort != nil {
				opt.recyclePort(opt.MediaPort)
			}
			channel.Error("inviteTcpCreateError", zap.Error(err))
			return http.StatusInternalServerError, err
		}

		if !opt.IsLive() {
			// 10秒无数据关闭
			if psPuber.Stream.DelayCloseTimeout == 0 {
				psPuber.Stream.DelayCloseTimeout = time.Second * 10
			}
			if psPuber.Stream.IdleTimeout == 0 {
				psPuber.Stream.IdleTimeout = time.Second * 10
			}
		}
		PullStreams.Store(streamPath, &PullStream{
			opt:       opt,
			channel:   channel,
			inviteRes: inviteRes,
		})
		err = srv.Send(sip.NewAckRequest("", invite, inviteRes, "", nil))
	} else {
		if opt.recyclePort != nil {
			opt.recyclePort(opt.MediaPort)
		}
	}
	return
}

func (channel *Channel) Bye(streamPath string) int {
	d := channel.Device
	if streamPath == "" {
		streamPath = fmt.Sprintf("%s/%s", d.ID, channel.DeviceID)
	}
	if s, loaded := PullStreams.LoadAndDelete(streamPath); loaded {
		s.(*PullStream).Bye()
		if s := Streams.Get(streamPath); s != nil {
			s.Close()
		}
		return http.StatusOK
	}
	return http.StatusNotFound
}

func (channel *Channel) Pause(streamPath string) int {
	if s, loaded := PullStreams.Load(streamPath); loaded {
		r := s.(*PullStream).Pause()
		if s := Streams.Get(streamPath); s != nil {
			s.Pause()
		}
		return r
	}
	return http.StatusNotFound
}

func (channel *Channel) Resume(streamPath string) int {
	if s, loaded := PullStreams.Load(streamPath); loaded {
		r := s.(*PullStream).Resume()
		if s := Streams.Get(streamPath); s != nil {
			s.Resume()
		}
		return r
	}
	return http.StatusNotFound
}

func (channel *Channel) PlayAt(streamPath string, second uint) int {
	if s, loaded := PullStreams.Load(streamPath); loaded {
		r := s.(*PullStream).PlayAt(second)
		if s := Streams.Get(streamPath); s != nil {
			s.Resume()
		}
		return r
	}
	return http.StatusNotFound
}

func (channel *Channel) PlayForward(streamPath string, speed float32) int {
	if s, loaded := PullStreams.Load(streamPath); loaded {
		return s.(*PullStream).PlayForward(speed)
	}
	if s := Streams.Get(streamPath); s != nil {
		s.Resume()
	}
	return http.StatusNotFound
}

func (channel *Channel) TryAutoInvite(opt *InviteOptions) {
	condition := !opt.IsLive() || channel.CanInvite()
	channel.Debug("TryAutoInvite", zap.Any("opt", opt), zap.Bool("condition", condition))
	if condition {
		go channel.Invite(opt)
	}
}

func (channel *Channel) CanInvite() bool {
	if channel.State.Load() != 0 || len(channel.DeviceID) != 20 || channel.Status == ChannelOffStatus {
		return false
	}

	if conf.InviteIDs == "" {
		return true
	}

	// 11～13位是设备类型编码
	typeID := channel.DeviceID[10:13]

	// format: start-end,type1,type2
	tokens := strings.Split(conf.InviteIDs, ",")
	for _, tok := range tokens {
		if first, second, ok := strings.Cut(tok, "-"); ok {
			if typeID >= first && typeID <= second {
				return true
			}
		} else {
			if typeID == first {
				return true
			}
		}
	}

	return false
}

func getSipRespErrorCode(err error) int {
	if re, ok := err.(*sip.RequestError); ok {
		return int(re.Code)
	} else {
		return http.StatusInternalServerError
	}
}
