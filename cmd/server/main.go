package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/linguo2625469/workbuddy2api-panel/internal/buildinfo"
	"github.com/linguo2625469/workbuddy2api-panel/internal/gateway"
)

// appVersion remains the canonical literal consumed by release workflows.
const appVersion = buildinfo.Version

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	instance, err := gateway.Start(ctx, gateway.Options{
		ConfigPath: *cfgPath,
		Version:    appVersion,
	})
	if err != nil {
		log.Printf("start gateway: %v", err)
		os.Exit(1)
	}
	defer func() {
		if err := instance.Close(); err != nil {
			log.Printf("gateway shutdown: %v", err)
		}
	}()

	<-ctx.Done()
	fmt.Printf("stopping %s\n", buildinfo.DesktopName)
}
