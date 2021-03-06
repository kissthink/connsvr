package fsvr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/simplejia/clog/api"
	"github.com/simplejia/connsvr/comm"
	"github.com/simplejia/connsvr/conf"
	"github.com/simplejia/connsvr/core"
	"github.com/simplejia/connsvr/proto"
	"github.com/simplejia/utils"
)

func Fserver(host string, t comm.PROTO) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", host)
	if err != nil {
		panic(err)
	}
	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		panic(err)
	}
	defer listener.Close()

	for {
		c, err := listener.AcceptTCP()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				time.Sleep(time.Millisecond)
				continue
			}
			clog.Error("Fserver() listener.AcceptTCP %v", err)
			os.Exit(-1)
		}

		c.SetReadBuffer(conf.C.Cons.C_RBUF)
		c.SetWriteBuffer(conf.C.Cons.C_WBUF)

		connWrap := &core.ConnWrap{
			T:  t,
			C:  c,
			BR: bufio.NewReaderSize(c, 128),
		}
		go frecv(connWrap)
	}
}

// 请赋值成自己的根据addrType, addr返回ip:port的函数
var PubAddrFunc = func(addrType, addr string) (string, error) {
	return addr, nil
}

func dispatchCmd(connWrap *core.ConnWrap, msg proto.Msg) {
	switch msg.Cmd() {
	case comm.PING:
		connWrap.Write(msg)
	case comm.ENTER:
		if msg.Uid() == "" || msg.Rid() == "" {
			break
		}

		// 不同用户不能复用同一个连接, 新用户替代老用户数据
		if connWrap.Uid != msg.Uid() || connWrap.Sid != msg.Sid() {
			for _, rid := range connWrap.Rids {
				core.RM.Del(rid, connWrap)
			}
			connWrap.Uid = msg.Uid()
			connWrap.Sid = msg.Sid()
			connWrap.Rids = []string{msg.Rid()}
			core.RM.Add(msg.Rid(), connWrap)
		} else {
			i := sort.SearchStrings(connWrap.Rids, msg.Rid())
			if i == len(connWrap.Rids) || connWrap.Rids[i] != msg.Rid() {
				if len(connWrap.Rids) >= conf.C.Cons.MAX_ROOM_NUM {
					msg.SetCmd(comm.ERR)
					connWrap.Write(msg)
					break
				}
				if i == len(connWrap.Rids) {
					connWrap.Rids = append(connWrap.Rids, msg.Rid())
				} else {
					connWrap.Rids = append(connWrap.Rids[:i], append([]string{msg.Rid()}, connWrap.Rids[i:]...)...)
				}
				core.RM.Add(msg.Rid(), connWrap)
			}
		}

		enterBody := &comm.EnterBody{}
		if body := msg.Body(); body != "" {
			err := json.Unmarshal([]byte(body), enterBody)
			if err != nil {
				clog.Error("fsvr:dispatchCmd() json.Unmarshal error: %v, data: %s", err, body)
				return
			}
		}

		mixBodys := map[byte][]string{}
		for subcmd, msgId := range enterBody.MsgIds {
			msg.SetSubcmd(subcmd)
			bodys := core.ML.Bodys(msgId, msg)
			if len(bodys) > 0 {
				mixBodys[subcmd] = bodys
			}
		}

		if len(mixBodys) > 0 {
			bs, _ := json.Marshal(mixBodys)
			msg.SetBody(string(bs))
			msg.SetCmd(comm.MSGS)
			connWrap.Write(msg)
		}
	case comm.LEAVE:
		if msg.Uid() == "" || msg.Rid() == "" {
			break
		}

		i := sort.SearchStrings(connWrap.Rids, msg.Rid())
		if i < len(connWrap.Rids) && connWrap.Rids[i] == msg.Rid() {
			connWrap.Rids = append(connWrap.Rids[:i], connWrap.Rids[i+1:]...)
		}
		core.RM.Del(msg.Rid(), connWrap)
	case comm.PUB:
		subcmd := strconv.Itoa(int(msg.Subcmd()))
		pub := conf.C.Pubs[subcmd]
		if pub == nil {
			clog.Error("fsvr:dispatchCmd() no expected subcmd: %s", subcmd)
			return
		}
		addr, err := PubAddrFunc(pub.AddrType, pub.Addr)
		if err != nil {
			clog.Error("fsvr:dispatchCmd() PubAddrFunc error: %v", err)
			return
		}
		arrs := []string{
			strconv.Itoa(int(msg.Cmd())),
			subcmd,
			msg.Uid(),
			msg.Sid(),
			msg.Rid(),
			url.QueryEscape(msg.Body()),
		}
		ps := map[string]string{}
		values, _ := url.ParseQuery(fmt.Sprintf(pub.Params, utils.Slice2Interface(arrs)...))
		for k, vs := range values {
			ps[k] = vs[0]
		}

		timeout, _ := time.ParseDuration(pub.Timeout)

		headers := map[string]string{
			"Host": pub.Host,
		}

		var cliExt *comm.CliExt
		if ext := msg.Ext(); ext != "" {
			err := json.Unmarshal([]byte(ext), &cliExt)
			if err != nil {
				clog.Error("fsvr:dispatchCmd() json.Unmarshal error: %v, data: %s", err, ext)
				return
			}
		}
		if cliExt != nil {
			headers["Cookie"] = cliExt.Cookie
		}

		uri := fmt.Sprintf("http://%s/%s", addr, strings.TrimPrefix(pub.Cgi, "/"))

		gpp := &utils.GPP{
			Uri:     uri,
			Timeout: timeout,
			Headers: headers,
			Params:  ps,
		}

		var body []byte
		step, maxstep := -1, 3
		if pub.Retry < maxstep {
			maxstep = pub.Retry
		}
		for ; step < maxstep; step++ {
			switch pub.Method {
			case "get":
				body, err = utils.Get(gpp)
			case "post":
				body, err = utils.Post(gpp)
			}

			if err != nil {
				clog.Error("fsvr:dispatchCmd() http error, err: %v, body: %s, gpp: %v, step: %d", err, body, gpp, step)
			} else {
				clog.Debug("fsvr:dispatchCmd() http success, body: %s, gpp: %v", body, gpp)
				break
			}
		}

		if step == maxstep {
			msg.SetCmd(comm.ERR)
		} else {
			msg.SetBody(string(body))
		}
		connWrap.Write(msg)
	case comm.MSGS:
		msgsBody := &comm.MsgsBody{}
		if body := msg.Body(); body != "" {
			err := json.Unmarshal([]byte(body), msgsBody)
			if err != nil {
				clog.Error("fsvr:dispatchCmd() json.Unmarshal error: %v, data: %s", err, body)
				return
			}
		}

		subcmdOrig := msg.Subcmd()

		mixBodys := map[byte][]string{}
		for subcmd, msgId := range msgsBody.MsgIds {
			msg.SetSubcmd(subcmd)
			bodys := core.ML.Bodys(msgId, msg)
			if len(bodys) > 0 {
				mixBodys[subcmd] = bodys
			}
		}

		bs, _ := json.Marshal(mixBodys)
		msg.SetBody(string(bs))
		msg.SetSubcmd(subcmdOrig)
		connWrap.Write(msg)
	default:
		clog.Warn("fsvr:dispatchCmd() unexpected cmd: %v", msg.Cmd())
	}

	return
}

func frecv(connWrap *core.ConnWrap) {
	for {
		msg := connWrap.Read()
		if msg == nil {
			return
		}

		clog.Debug("frecv() connWrap.Read %+v", msg)
		dispatchCmd(connWrap, msg)
	}
}
