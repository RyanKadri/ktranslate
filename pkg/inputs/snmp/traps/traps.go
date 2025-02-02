package traps

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gosnmp/gosnmp"

	"github.com/kentik/ktranslate/pkg/eggs/logger"
	"github.com/kentik/ktranslate/pkg/inputs/snmp/mibs"
	snmp_util "github.com/kentik/ktranslate/pkg/inputs/snmp/util"
	"github.com/kentik/ktranslate/pkg/kt"
	"github.com/kentik/ktranslate/pkg/util/resolv"
)

const (
	snmpTrapOID   = ".1.3.6.1.6.3.1.1.4.1"
	snmpTrapOID_0 = ".1.3.6.1.6.3.1.1.4.1.0"
)

type SnmpTrap struct {
	log       logger.ContextL
	jchfChan  chan []*kt.JCHF
	listen    string
	tl        *gosnmp.TrapListener
	metrics   *kt.SnmpMetricSet
	conf      *kt.SnmpConfig
	mux       sync.RWMutex
	mibdb     *mibs.MibDB
	resolv    *resolv.Resolver
	ctx       context.Context
	deviceMap map[string]*kt.SnmpDeviceConfig
	baseTags  map[string]string
}

// Move to util?
type logWrapper struct {
	print  func(v ...interface{})
	printf func(format string, v ...interface{})
}

func (l logWrapper) Print(v ...interface{}) {
	l.print(v...)
}

func (l logWrapper) Printf(format string, v ...interface{}) {
	l.printf(format, v...)
}

func NewSnmpTrapListener(ctx context.Context, conf *kt.SnmpConfig, jchfChan chan []*kt.JCHF, metrics *kt.SnmpMetricSet, mibdb *mibs.MibDB,
	log logger.ContextL, resolv *resolv.Resolver, serviceName string, globalTags map[string]string) (*SnmpTrap, error) {
	st := &SnmpTrap{
		jchfChan:  jchfChan,
		log:       log,
		mibdb:     mibdb,
		listen:    conf.Trap.Listen,
		metrics:   metrics,
		deviceMap: map[string]*kt.SnmpDeviceConfig{},
		resolv:    resolv,
		ctx:       ctx,
		conf:      conf,
	}

	// Some quick defaults.
	if conf.Trap.Transport == "" {
		conf.Trap.Transport = "udp"
	}
	if conf.Trap.Community == "" {
		conf.Trap.Community = "hello"
	}
	if conf.Global.TimeoutMS == 0 {
		conf.Global.TimeoutMS = 5000
	}

	// Now set things up.
	tl := gosnmp.NewTrapListener()
	tl.OnNewTrap = st.handle
	tl.Params = &gosnmp.GoSNMP{
		Transport:          conf.Trap.Transport,
		Community:          conf.Trap.Community,
		Timeout:            time.Duration(conf.Global.TimeoutMS) * time.Millisecond,
		Retries:            3,
		ExponentialTimeout: true,
		MaxOids:            gosnmp.MaxOids,
	}
	switch conf.Trap.Version {
	case "v1":
		tl.Params.Version = gosnmp.Version1
	case "v2c", "":
		tl.Params.Version = gosnmp.Version2c
	case "v3":
		params, flags, contextEngineID, contextName, err := snmp_util.ParseV3Config(conf.Trap.V3)
		if err != nil {
			return nil, err
		}
		tl.Params.Version = gosnmp.Version3
		tl.Params.SecurityModel = gosnmp.UserSecurityModel
		tl.Params.MsgFlags = flags
		tl.Params.SecurityParameters = params
		tl.Params.ContextEngineID = contextEngineID
		tl.Params.ContextName = contextName
	default:
		return nil, fmt.Errorf("Invalid trap version: %s", conf.Trap.Version)
	}

	tl.Params.Logger = gosnmp.NewLogger(logWrapper{
		print: func(v ...interface{}) {
			log.Debugf("GoSNMP Trap:" + fmt.Sprint(v...))
		},
		printf: func(format string, v ...interface{}) {
			log.Debugf("GoSNMP Trap:  "+format, v...)
		},
	})
	st.tl = tl
	log.Infof("Trap listener setup with version %s on %s.", conf.Trap.Version, conf.Trap.Listen)

	for _, device := range conf.Devices {
		st.deviceMap[device.DeviceIP] = device
	}

	// Set up some default tags if the device sending isn't found.
	baseTags := map[string]string{}
	if serviceName != "" {
		baseTags[kt.UserTagPrefix+"container_service"] = serviceName
	}
	for k, v := range globalTags {
		key := k
		if !strings.HasPrefix(key, kt.UserTagPrefix) {
			key = kt.UserTagPrefix + k
		}
		baseTags[key] = v
	}
	st.baseTags = baseTags

	return st, nil
}

func (s *SnmpTrap) Listen() {
	err := s.tl.Listen(s.listen)
	if err != nil {
		s.log.Errorf("error in Trap listen: %s", err)
	}
}

func (s *SnmpTrap) handle(packet *gosnmp.SnmpPacket, addr *net.UDPAddr) {
	s.log.Infof("got trapdata from %s", addr.IP)
	s.metrics.Traps.Mark(1)
	s.mux.RLock()
	defer s.mux.RUnlock()

	dev := s.deviceMap[addr.IP.String()] // See if we know which device this is coming from.
	dst := kt.NewJCHF()
	dst.CustomStr = make(map[string]string)
	dst.CustomInt = make(map[string]int32)
	dst.CustomBigInt = make(map[string]int64)
	dst.EventType = kt.KENTIK_EVENT_SNMP_TRAP
	dst.SrcAddr = addr.IP.String()
	if dev != nil {
		dst.DeviceName = dev.DeviceName
		if s.conf.Trap.TrapOnly {
			dst.Provider = kt.ProviderTrapUnknown
		} else {
			dst.Provider = dev.Provider
		}
		dev.SetUserTags(dst.CustomStr)
	} else {
		dst.DeviceName = addr.IP.String()
		if s.resolv != nil { // Try pulling device name from reverse IP if possible.
			dm := s.resolv.Resolve(s.ctx, dst.DeviceName, true)
			if dm != "" {
				dst.DeviceName = dm
			}
		}
		dst.Provider = kt.ProviderTrapUnknown
		for k, v := range s.baseTags {
			dst.CustomStr[k] = v
		}
	}

	// What trap is this from?
	var trap *mibs.Trap
	for _, v := range packet.Variables {
		if v.Name == snmpTrapOID || v.Name == snmpTrapOID_0 {
			if v.Type == gosnmp.ObjectIdentifier {
				toid := v.Value.(string)
				trap = s.mibdb.GetTrap(toid)
				dst.CustomStr["TrapOID"] = toid
				if trap != nil {
					dst.CustomStr["TrapName"] = trap.Name
					idx := snmp_util.GetIndex(toid, trap.Oid)
					if idx != "" {
						dst.CustomStr["Index"] = idx
					}
				}
			}
		}
	}

	for _, v := range packet.Variables {
		if v.Name == snmpTrapOID || v.Name == snmpTrapOID_0 {
			continue
		}

		// Do we know this guy?
		res, err := s.mibdb.GetForKey(v.Name)
		if err != nil {
			s.log.Errorf("Cannot look up OID in trap: %v", err)
		}

		// If we don't want undefined vars, pass along here.
		if res == nil && trap.DropUndefinedVars() {
			continue
		}

		switch v.Type {
		case gosnmp.OctetString:
			if value, ok := snmp_util.ReadOctetString(v, snmp_util.NO_TRUNCATE); ok {
				if res != nil && res.Conversion != "" { // Adjust for any hard coded values here.
					_, value, _ = snmp_util.GetFromConv(v, res.Conversion, s.log)
				}
				if !utf8.ValidString(value) { // Print out value as a hex string.
					value = fmt.Sprintf("%x", v.Value)
				}
				if res != nil {
					dst.CustomStr[res.GetName()] = value
				} else {
					dst.CustomStr[v.Name] = value
				}
			}
		case gosnmp.Counter64, gosnmp.Counter32, gosnmp.Gauge32, gosnmp.TimeTicks, gosnmp.Uinteger32, gosnmp.Integer:
			if res != nil {
				dst.CustomBigInt[res.GetName()] = gosnmp.ToBigInt(v.Value).Int64()
			} else {
				dst.CustomBigInt[v.Name] = gosnmp.ToBigInt(v.Value).Int64()
			}
		case gosnmp.ObjectIdentifier:
			value := v.Value.(string)
			if res != nil && res.Conversion != "" { // Adjust for any hard coded values here.
				_, value, _ = snmp_util.GetFromConv(v, res.Conversion, s.log)
			}
			if !utf8.ValidString(value) { // Print out value as a hex string.
				value = fmt.Sprintf("%x", v.Value)
			}
			if res != nil {
				dst.CustomStr[res.GetName()] = value
			} else {
				dst.CustomStr[v.Name] = value
			}
		default:
			s.log.Infof("trap variable with unknown type (%v) handling, skipping: %+v", v.Type, v)
		}
	}

	s.jchfChan <- []*kt.JCHF{dst}
}
