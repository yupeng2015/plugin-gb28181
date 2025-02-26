package gb28181

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ghettovoice/gosip/sip"
	myip "github.com/husanpao/ip"
	"go.uber.org/zap"
	. "m7s.live/engine/v4"
	"m7s.live/engine/v4/util"
)

type GB28181PositionConfig struct {
	AutosubPosition bool          `desc:"是否自动订阅定位"`             //是否自动订阅定位
	Expires         time.Duration `default:"3600s" desc:"订阅周期"` //订阅周期
	Interval        time.Duration `default:"6s" desc:"订阅间隔"`    //订阅间隔
}

type GB28181Config struct {
	InviteMode int    `default:"1" desc:"拉流模式" enum:"0:手动拉流,1:预拉流,2:按需拉流"`      //邀请模式，0:手动拉流，1:预拉流，2:按需拉流
	InviteIDs  string `default:"131,132" desc:"允许邀请的设备类型（ 11～13位是设备类型编码）,逗号分割"` //按照国标gb28181协议允许邀请的设备类型:132 摄像机 NVR
	ListenAddr string `default:"0.0.0.0" desc:"监听IP地址"`                         //监听地址
	//sip服务器的配置
	SipNetwork string   `default:"udp"  desc:"废弃，请使用 Port"`               //传输协议，默认UDP，可选TCP
	SipIP      string   `desc:"sip 服务IP地址"`                               //sip 服务器公网IP
	SipPort    sip.Port `default:"5060" desc:"废弃，请使用 Port"`               //sip 服务器端口，默认 5060
	Serial     string   `default:"34020000002000000001" desc:"sip 服务 id"` //sip 服务器 id, 默认 34020000002000000001
	Realm      string   `default:"3402000000" desc:"sip 服务域"`             //sip 服务器域，默认 3402000000
	Username   string   `desc:"sip 服务账号"`                                 //sip 服务器账号
	Password   string   `desc:"sip 服务密码"`                                 //sip 服务器密码
	Port       struct { // 新配置方式
		Sip   string `default:"udp:5060" desc:"sip服务端口号"`
		Media string `default:"tcp:58200-59200" desc:"媒体服务端口号"`
		Fdm   bool   `default:"false" desc:"多路复用"`
	}
	RegisterValidity  time.Duration `default:"3600s" desc:"注册有效期"` //注册有效期，单位秒，默认 3600
	HeartbeatInterval time.Duration `default:"60s" desc:"心跳间隔"`    //心跳间隔，单位秒，默认 60

	//媒体服务器配置
	MediaIP      string `desc:"媒体服务IP地址"`                    //媒体服务器地址
	MediaPort    uint16 `default:"58200" desc:"废弃，请使用 Port"` //媒体服务器端口
	MediaNetwork string `default:"tcp" desc:"废弃，请使用 Port"`   //媒体传输协议，默认UDP，可选TCP
	MediaPortMin uint16 `default:"58200" desc:"废弃，请使用 Port"`
	MediaPortMax uint16 `default:"59200" desc:"废弃，请使用 Port"`

	RemoveBanInterval time.Duration `default:"600s"  desc:"移除禁止设备间隔"` //移除禁止设备间隔
	routes            map[string]string
	DumpPath          string   `desc:"dump PS流本地文件路径"` //dump PS流本地文件路径
	Ignores           []string `desc:"忽略的设备ID"`        //忽略的设备ID
	ignores           map[string]struct{}
	tcpPorts          PortManager
	udpPorts          PortManager

	Position GB28181PositionConfig //关于定位的配置参数

}

var SipUri *sip.SipUri

func (c *GB28181Config) initRoutes() {
	c.routes = make(map[string]string)
	tempIps := myip.LocalAndInternalIPs()
	for k, v := range tempIps {
		c.routes[k] = v
		if lastdot := strings.LastIndex(k, "."); lastdot >= 0 {
			c.routes[k[0:lastdot]] = k
		}
	}
	GB28181Plugin.Info("LocalAndInternalIPs", zap.Any("routes", c.routes))
}

func (c *GB28181Config) OnEvent(event any) {
	switch e := event.(type) {
	case FirstConfig:
		if c.Port.Sip != "udp:5060" {
			protocol, ports := util.Conf2Listener(c.Port.Sip)
			c.SipNetwork = protocol
			c.SipPort = sip.Port(ports[0])
		}
		if c.Port.Media != "tcp:58200-59200" {
			protocol, ports := util.Conf2Listener(c.Port.Media)
			c.MediaNetwork = protocol
			if len(ports) > 1 {
				c.MediaPortMin = ports[0]
				c.MediaPortMax = ports[1]
			} else {
				c.MediaPortMin = 0
				c.MediaPortMax = 0
				c.MediaPort = ports[0]
			}
		}
		if len(c.Ignores) > 0 {
			c.ignores = make(map[string]struct{})
			for _, v := range c.Ignores {
				c.ignores[v] = util.Null
			}
		}
		os.MkdirAll(c.DumpPath, 0766)
		c.ReadDevices()
		SipUri = &sip.SipUri{
			FUser: sip.String{Str: c.Serial},
			FHost: c.SipIP,
			FPort: &conf.SipPort,
		}
		go c.initRoutes()
		c.startServer()
	case InvitePublish:
		if c.InviteMode == INVIDE_MODE_ONSUBSCRIBE {
			//流可能是回放流，stream path是device/channel/start-end形式
			streamNames := strings.Split(e.Target, "/")
			if channel := FindChannel(streamNames[0], streamNames[1]); channel != nil {
				opt := InviteOptions{}
				if len(streamNames) > 2 {
					last := len(streamNames) - 1
					timestr := streamNames[last]
					trange := strings.Split(timestr, "-")
					if len(trange) == 2 {
						startTime := trange[0]
						endTime := trange[1]
						opt.Validate(startTime, endTime)
					}
				}
				channel.TryAutoInvite(&opt)
			}
		}
	case SEpublish:
		if channel := FindChannel(e.Target.AppName, strings.TrimSuffix(e.Target.StreamName, "/rtsp")); channel != nil {
			channel.LiveSubSP = e.Target.Path
		}
	case SEclose:
		if channel := FindChannel(e.Target.AppName, strings.TrimSuffix(e.Target.StreamName, "/rtsp")); channel != nil {
			channel.LiveSubSP = ""
		}
		if v, ok := PullStreams.LoadAndDelete(e.Target.Path); ok {
			go v.(*PullStream).Bye()
		}
	}
}

func (c *GB28181Config) IsMediaNetworkTCP() bool {
	return strings.ToLower(c.MediaNetwork) == "tcp"
}

var conf GB28181Config

var GB28181Plugin = InstallPlugin(&conf)
var PullStreams sync.Map //拉流
