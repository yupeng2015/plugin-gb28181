package gb28181

import (
	"encoding/hex"
	"encoding/xml"
	"fmt"
)

var (
	name2code = map[string]uint8{
		"stop":      0,
		"right":     1,
		"left":      2,
		"down":      4,
		"downright": 5,
		"downleft":  6,
		"up":        8,
		"upright":   9,
		"upleft":    10,
		"zoomin":    16,
		"zoomout":   32,
	}
)

type PresetCmd byte

const (
	PresetAddPoint  = 0
	PresetDelPoint  = 1
	PresetCallPoint = 2
)

const DeviceControl = "DeviceControl"
const PTZFirstByte = 0xA5
const (
	PresetSet  = 0x81
	PresetCall = 0x82
	PresetDel  = 0x83
)

type MessagePtz struct {
	XMLName  xml.Name `xml:"Control"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
	PTZCmd   string   `xml:"PTZCmd"`
}

type Preset struct {
	CMD   byte
	Point byte
}

func toPtzStrByCmdName(cmdName string, horizontalSpeed, verticalSpeed, zoomSpeed uint8) (string, error) {
	c, err := toPtzCode(cmdName)
	if err != nil {
		return "", err
	}
	return toPtzStr(c, horizontalSpeed, verticalSpeed, zoomSpeed), nil
}

func toPtzStr(cmdCode, horizontalSpeed, verticalSpeed, zoomSpeed uint8) string {
	checkCode := uint16(0xA5+0x0F+0x01+cmdCode+horizontalSpeed+verticalSpeed+(zoomSpeed&0xF0)) % 0x100

	return fmt.Sprintf("A50F01%02X%02X%02X%01X0%02X",
		cmdCode,
		horizontalSpeed,
		verticalSpeed,
		zoomSpeed>>4, // 根据 GB28181 协议，zoom 只取 4 bit
		checkCode,
	)
}

func toPtzCode(cmd string) (uint8, error) {
	if code, ok := name2code[cmd]; ok {
		return code, nil
	} else {
		return 0, fmt.Errorf("invalid ptz cmd %q", cmd)
	}
}

func getVerificationCode(ptz []byte) {
	sum := uint8(0)
	for i := 0; i < len(ptz)-1; i++ {
		sum += ptz[i]
	}
	ptz[len(ptz)-1] = sum
}

func getAssembleCode() uint8 {
	return (PTZFirstByte>>4 + PTZFirstByte&0xF + 0) % 16
}

func Pack(cmd, point byte) string {
	buf := make([]byte, 8)
	buf[0] = PTZFirstByte
	buf[1] = getAssembleCode()
	buf[2] = 0

	buf[3] = cmd

	buf[4] = 0
	buf[5] = point
	buf[6] = 0
	getVerificationCode(buf)
	return hex.EncodeToString(buf)
}
