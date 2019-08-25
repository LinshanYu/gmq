package gnode

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/wuzhc/gmq/configs"
	"github.com/wuzhc/gmq/pkg/logs"
	"github.com/wuzhc/gmq/pkg/utils"

	"gopkg.in/ini.v1"
)

type Gnode struct {
	running  int32
	exitChan chan struct{}
	ctx      context.Context
	wg       utils.WaitGroupWrapper
	cfg      *configs.GnodeConfig
}

func New() *Gnode {
	return &Gnode{
		ctx:      context.Background(),
		exitChan: make(chan struct{}),
	}
}

func (gn *Gnode) Run() {
	defer gn.wg.Wait()

	if atomic.LoadInt32(&gn.running) == 1 {
		log.Fatalln("Gnode is running.")
	}
	if !atomic.CompareAndSwapInt32(&gn.running, 0, 1) {
		log.Fatalln("Gnode start failed.")
	}

	if gn.cfg == nil {
		gn.SetDefaultConfig()
	}

	ctx := &Context{
		Gnode:   gn,
		Conf:    gn.cfg,
		Logger:  gn.initLogger(),
		RedisDB: gn.initRedisPool(),
	}

	gn.wg.Wrap(NewDispatcher(ctx).Run)
	gn.wg.Wrap(NewHttpServ(ctx).Run)
	gn.wg.Wrap(NewTcpServ(ctx).Run)

	if err := gn.register(); err != nil {
		log.Fatalln("Register failed, ", err)
	}

	gn.installSignalHandler()
	ctx.Logger.Info("Gnode is running.")
}

func (gn *Gnode) Exit() {
	if err := gn.unregister(); err != nil {
		log.Fatalln("failed")
	}

	close(gn.exitChan)
}

func (gn *Gnode) SetConfig(cfgFile string) {
	if res, err := utils.PathExists(cfgFile); !res {
		if err != nil {
			log.Fatalf("%s is not exists,errors:%s \n", cfgFile, err.Error())
		} else {
			log.Fatalf("%s is not exists \n", cfgFile)
		}
	}

	c, err := ini.Load(cfgFile)
	if err != nil {
		log.Fatalf("Fail to read file: %v \n", err)
	}

	cfg := new(configs.GnodeConfig)

	// node
	cfg.NodeId, _ = c.Section("node").Key("id").Int64()

	// log config
	cfg.LogFilename = c.Section("log").Key("filename").String()
	cfg.LogLevel, _ = c.Section("log").Key("level").Int()
	cfg.LogRotate, _ = c.Section("log").Key("rotate").Bool()
	cfg.LogMaxSize, _ = c.Section("log").Key("max_size").Int()
	cfg.LogTargetType = c.Section("log").Key("target_type").String()

	// redis config
	cfg.RedisHost = c.Section("redis").Key("host").String()
	cfg.RedisPwd = c.Section("redis").Key("pwd").String()
	cfg.RedisPort = c.Section("redis").Key("port").String()
	cfg.RedisMaxIdle, _ = c.Section("redis").Key("max_idle").Int()
	cfg.RedisMaxActive, _ = c.Section("redis").Key("max_active").Int()

	// bucket config
	cfg.BucketNum, _ = c.Section("bucket").Key("num").Int()
	cfg.TTRBucketNum, _ = c.Section("TTRBucket").Key("num").Int()

	// http server config
	httpServAddr := c.Section("http_server").Key("addr").String()
	cfg.HttpServCertFile = c.Section("http_server").Key("certFile").String()
	cfg.HttpServKeyFile = c.Section("http_server").Key("keyFile").String()
	cfg.HttpServEnableTls, _ = c.Section("http_server").Key("enableTls").Bool()

	// tcp server config
	tcpServAddr := c.Section("tcp_server").Key("addr").String()
	cfg.TcpServCertFile = c.Section("tcp_server").Key("certFile").String()
	cfg.TcpServKeyFile = c.Section("tcp_server").Key("keyFile").String()
	cfg.TcpServEnableTls, _ = c.Section("tcp_server").Key("enableTls").Bool()
	cfg.TcpServWeight, _ = c.Section("tcp_server").Key("weight").Int()

	// register config
	cfg.GregisterAddr = c.Section("gregister").Key("addr").String()

	// parse flag
	flag.StringVar(&cfg.TcpServAddr, "tcp_addr", tcpServAddr, "tcp address")
	flag.StringVar(&cfg.HttpServAddr, "http_addr", httpServAddr, "http address")
	flag.Parse()

	gn.cfg = cfg
	gn.cfg.SetDefault()
}

func (gn *Gnode) SetDefaultConfig() {
	cfg := new(configs.GnodeConfig)

	flag.StringVar(&gn.cfg.TcpServAddr, "tcp_addr", "", "tcp address")
	flag.StringVar(&gn.cfg.HttpServAddr, "http_addr", "", "http address")
	flag.Parse()

	gn.cfg = cfg
	gn.cfg.SetDefault()
}

func (gn *Gnode) installSignalHandler() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		<-sigs
		gn.Exit()
	}()
}

func (gn *Gnode) initLogger() *logs.Dispatcher {
	logger := logs.NewDispatcher()
	targets := strings.Split(gn.cfg.LogTargetType, ",")
	for _, t := range targets {
		if t == logs.TARGET_FILE {
			conf := fmt.Sprintf(`{"filename":"%s","level":%d,"max_size":%d,"rotate":%v}`, gn.cfg.LogFilename, gn.cfg.LogLevel, gn.cfg.LogMaxSize, gn.cfg.LogRotate)
			logger.SetTarget(logs.TARGET_FILE, conf)
		} else if t == logs.TARGET_CONSOLE {
			logger.SetTarget(logs.TARGET_CONSOLE, "")
		} else {
			log.Fatalln("Only support file or console handler")
		}
	}
	return logger
}

func (gn *Gnode) initRedisPool() *RedisDB {
	return Redis.InitPool(gn.cfg)
}

type rs struct {
	Code int         `json:"code"`
	Data interface{} `json:"data"`
	Msg  string      `json:"msg"`
}

func (gn *Gnode) register() error {
	hosts := strings.Split(gn.cfg.GregisterAddr, ",")
	for _, host := range hosts {
		url := fmt.Sprintf("%s/register?tcp_addr=%s&http_addr=%s&weight=%d&node_id=%d", host, gn.cfg.TcpServAddr, gn.cfg.HttpServAddr, gn.cfg.TcpServWeight, gn.cfg.NodeId)
		resp, err := http.Get(url)
		if err != nil {
			return err
		}
		res, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		var r rs
		if err := json.Unmarshal(res, &r); err != nil {
			log.Fatalln(err)
		}
		if r.Code == 1 {
			log.Fatalln(r.Msg)
		}
	}

	return nil
}

func (gn *Gnode) unregister() error {
	ts := strings.Split(gn.cfg.GregisterAddr, ",")
	for _, t := range ts {
		url := t + "/unregister?tcp_addr=" + gn.cfg.TcpServAddr
		resp, err := http.Get(url)
		if err != nil {
			return err
		}
		res, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		var r rs
		if err := json.Unmarshal(res, &r); err != nil {
			log.Fatalln(err)
		}
		if r.Code == 1 {
			log.Fatalln(r.Msg)
		}
	}

	return nil
}