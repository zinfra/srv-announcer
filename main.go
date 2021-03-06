package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	checker "github.com/zinfra/srv-announcer/checker"
	tcpHealthcheck "github.com/zinfra/srv-announcer/checker/healthchecks/tcp"
	config "github.com/zinfra/srv-announcer/config"
	dns "github.com/zinfra/srv-announcer/dns"
	dummy "github.com/zinfra/srv-announcer/dns/dummy"
	route53 "github.com/zinfra/srv-announcer/dns/route53"
)

func main() {
	var config config.Config
	config.SRVRecord = &net.SRV{}
	var checkTarget, logLevelStr string
	var TTL, srvPort, srvPriority, srvWeight uint

	app := cli.NewApp()
	app.Name = "srv-announcer"
	app.Usage = "Sidecar managing DNS records in an SRV record set (RFC2782), a poormans alternative to proper service discovery"

	app.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "log-level",
			Aliases:     []string{"l"},
			Usage:       "The level at which to log (trace|debug|info|warn|error|fatal|panic), defaults to info",
			EnvVars:     []string{"SRV_ANNOUNCER_LOG_LEVEL"},
			Destination: &logLevelStr,
			Value:       "info",
		},
		&cli.BoolFlag{
			Name:        "dry-run",
			Usage:       "Don't actually update DNS, only log what would be done",
			EnvVars:     []string{"SRV_ANNOUNCER_DRY_RUN"},
			Destination: &config.DryRun,
		},
		&cli.StringFlag{
			Name:        "zone-name",
			Usage:       "Name of the Route53 Zone the records to manage are in",
			EnvVars:     []string{"SRV_ANNOUNCER_ZONE_NAME"},
			Destination: &config.ZoneName,
		},
		&cli.StringFlag{
			Name:        "srv-record-name",
			Usage:       "RFC2782 Name (_service._proto.name)",
			EnvVars:     []string{"SRV_ANNOUNCER_SRV_RECORD_NAME"},
			Destination: &config.SRVRecordName,
			Required:    true,
		},
		&cli.UintFlag{
			Name:        "srv-record-ttl",
			Usage:       "TTL of the RFC2782 SRV Record Set in seconds",
			EnvVars:     []string{"SRV_ANNOUNCER_SRV_RECORD_TTL"},
			Destination: &TTL,
			Value:       60,
		},
		&cli.UintFlag{
			Name:        "srv-record-priority",
			Usage:       "Priority of the RFC2782 SRV Record",
			EnvVars:     []string{"SRV_ANNOUNCER_SRV_RECORD_PRIORITY"},
			Destination: &srvPriority,
			Value:       10,
		},
		&cli.UintFlag{
			Name:        "srv-record-weight",
			Usage:       "Weight of the RFC2782 SRV Record",
			EnvVars:     []string{"SRV_ANNOUNCER_SRV_RECORD_WEIGHT"},
			Destination: &srvWeight,
			Value:       10,
		},
		&cli.UintFlag{
			Name:        "srv-record-port",
			Usage:       "Port of the RFC2782 SRV Record",
			EnvVars:     []string{"SRV_ANNOUNCER_SRV_RECORD_PORT"},
			Destination: &srvPort,
			Value:       443,
		},
		&cli.StringFlag{
			Name:        "srv-record-target",
			Usage:       "Target of the RFC2782 SRV Record. Usually something resembling your hostname, ending with a dot",
			EnvVars:     []string{"SRV_ANNOUNCER_SRV_RECORD_TARGET"},
			Destination: &config.SRVRecord.Target,
			Required:    true,
		},
		&cli.StringFlag{
			Name:        "check-target",
			Usage:       "hostname:port to check. Will derive from $srv-record-target and $srv-record-port if unspecified",
			EnvVars:     []string{"SRV_ANNOUNCER_CHECK_TARGET"},
			Destination: &checkTarget,
			Value:       "",
		},
		&cli.DurationFlag{
			Name:        "check-interval",
			Usage:       "Interval between checks",
			EnvVars:     []string{"SRV_ANNOUNCER_CHECK_INTERVAL"},
			Destination: &config.CheckInterval,
			Value:       time.Duration(10) * time.Second,
		},
		&cli.DurationFlag{
			Name:        "check-timeout",
			Usage:       "Timeout for each check",
			EnvVars:     []string{"SRV_ANNOUNCER_CHECK_TIMEOUT"},
			Destination: &config.CheckTimeout,
			Value:       time.Duration(1) * time.Second,
		},
	}

	app.Action = func(ctx *cli.Context) error {
		// configure logging
		logLevel, err := log.ParseLevel(logLevelStr)
		if err != nil {
			log.Errorf("Unable to parse log level: %s", err.Error())
			return err
		}
		log.SetLevel(logLevel)
		log.SetFormatter(&log.TextFormatter{})
		log.SetOutput(os.Stdout)


		// there's no uint16flag, so scan into uint and convert here.
		config.TTL = uint16(TTL)
		config.SRVRecord.Port = uint16(srvPort)
		config.SRVRecord.Priority = uint16(srvPriority)
		config.SRVRecord.Weight = uint16(srvWeight)

		if config.SRVRecord.Target[len(config.SRVRecord.Target)-1] == '.' {
			log.Warnf("The target of the RFC2782 SRV Record doesn't end with a dot, " +
				"which is probably not what you want. Continuing anyway.")
		}

		// fill checkTarget from SRVRecord.Target and SRVRecord.Port if it's not set
		if checkTarget == "" {
			checkTarget = fmt.Sprintf("%s:%d", strings.TrimRight(config.SRVRecord.Target, "."), srvPort)
			log.Infof("Check target %s derived from $srv-record-target and $srv-record-port", checkTarget)
		}
		config.CheckTarget = checkTarget

		var srvRecordManager dns.ISRVManager
		if config.DryRun {
			srvRecordManager = &dummy.SrvManager{}
		} else {
			// ensure config.ZoneName is set
			if config.ZoneName == "" {
				return errors.New("Zone name needs to be specified")
			}
			// initialize route53
			r53 := route53.NewClient()

			// lookup zone
			hostedZone, err := r53.GetZoneByName(config.ZoneName)
			if err != nil {
				return err
			}
			zoneID := aws.StringValue(hostedZone.Id)

			srvRecordManager = route53.NewSRVManager(r53, zoneID, config.SRVRecordName, config.TTL)
		}

		tcpHealthcheck := tcpHealthcheck.NewHealthcheck(config.CheckTarget, config.CheckTimeout, config.CheckInterval)

		return checker.Run(ctx.Context, tcpHealthcheck, config.SRVRecord, srvRecordManager)
	}

	// setup context
	ctx, cancelCtx := context.WithCancel(context.Background())

	// setup close handler
	interruptC := make(chan os.Signal)
	signal.Notify(interruptC, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-interruptC
		cancelCtx()
	}()

	err := app.RunContext(ctx, os.Args)

	if err != nil {
		log.Fatal(err)
	}
}
