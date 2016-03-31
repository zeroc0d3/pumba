package main // import "github.com/gaia-adm/pumba"

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gaia-adm/pumba/action"
	"github.com/gaia-adm/pumba/container"

	log "github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
)

var (
	wg      sync.WaitGroup
	client  container.Client
	cleanup bool
)

const (
	defaultKillSignal = "SIGKILL"
	re2prefix         = "re2:"
)

type commandT struct {
	pattern string
	names   []string
	command string
	signal  string
}

func init() {
	log.SetLevel(log.InfoLevel)
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

func main() {
	rootCertPath := "/etc/ssl/docker"

	if os.Getenv("DOCKER_CERT_PATH") != "" {
		rootCertPath = os.Getenv("DOCKER_CERT_PATH")
	}

	app := cli.NewApp()
	app.Name = "pumba"
	app.Usage = "Pumba is a resiliency tool that helps applications tolerate random Docker conainer failures."
	app.Before = before
	app.Action = start
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "host, H",
			Usage:  "daemon socket to connect to",
			Value:  "unix:///var/run/docker.sock",
			EnvVar: "DOCKER_HOST",
		},
		cli.BoolFlag{
			Name:  "tls",
			Usage: "use TLS; implied by --tlsverify",
		},
		cli.BoolFlag{
			Name:   "tlsverify",
			Usage:  "use TLS and verify the remote",
			EnvVar: "DOCKER_TLS_VERIFY",
		},
		cli.StringFlag{
			Name:  "tlscacert",
			Usage: "trust certs signed only by this CA",
			Value: fmt.Sprintf("%s/ca.pem", rootCertPath),
		},
		cli.StringFlag{
			Name:  "tlscert",
			Usage: "client certificate for TLS authentication",
			Value: fmt.Sprintf("%s/cert.pem", rootCertPath),
		},
		cli.StringFlag{
			Name:  "tlskey",
			Usage: "client key for TLS authentication",
			Value: fmt.Sprintf("%s/key.pem", rootCertPath),
		},
		cli.BoolFlag{
			Name:  "debug",
			Usage: "enable debug mode with verbose logging",
		},
		cli.StringSliceFlag{
			Name:  "chaos_cmd",
			Usage: "chaos command: `container(s,)/re2:regex|interval(s/m/h postfix)|STOP/KILL(:SIGNAL)/RM`",
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func before(c *cli.Context) error {
	if c.GlobalBool("debug") {
		log.SetLevel(log.DebugLevel)
	}

	cleanup = c.GlobalBool("cleanup")

	// Set-up container client
	tls, err := tlsConfig(c)
	if err != nil {
		return err
	}

	client = container.NewClient(c.GlobalString("host"), tls, !c.GlobalBool("no-pull"))

	handleSignals()
	return nil
}

func start(c *cli.Context) {
	if err := actions.CheckPrereqs(client, cleanup); err != nil {
		log.Fatal(err)
	}
	if err := createChaos(actions.Pumba{}, c.GlobalStringSlice("chaos_cmd"), 0); err != nil {
		log.Fatal(err)
	}
}

func createChaos(chaos actions.Chaos, args []string, limit int) error {
	// docker channel to pass all "stop" commands to
	dc := make(chan commandT)

	// range over all chaos arguments
	for _, chaosArg := range args {
		s := strings.Split(chaosArg, "|")
		if len(s) != 3 {
			return errors.New("Unexpected format for chaos_arg: use | separated triple")
		}
		// get container name pattern
		var pattern string
		var names []string
		if strings.HasPrefix(s[0], re2prefix) {
			pattern = strings.Trim(s[0], re2prefix)
			fmt.Println("Pattern: ", pattern)
		} else {
			names = strings.Split(s[0], ",")
		}
		// get interval duration
		interval, err := time.ParseDuration(s[1])
		if err != nil {
			return err
		}
		// get command and signal (if specified); convert everything to upper case
		cs := strings.Split(strings.ToUpper(s[2]), ":")
		command := cs[0]
		if !stringInSlice(command, []string{"STOP", "KILL", "RM"}) {
			return errors.New("Unexpected command in chaos_arg: can be STOP, KILL or RM")
		}
		signal := defaultKillSignal
		if len(cs) == 2 {
			signal = cs[1]
		}

		ticker := time.NewTicker(interval)
		go func(cmd commandT) {
			for range ticker.C {
				dc <- cmd
			}
		}(commandT{pattern, names, command, signal})

		for range dc {
			cmd := <-dc
			limit--
			if limit == 0 {
				ticker.Stop()
				close(dc)
			}
			wg.Add(1)
			go func(cmd commandT) {
				defer wg.Done()
				var err error
				switch cmd.command {
				case "STOP":
					if pattern == "" {
						err = chaos.StopByName(client, cmd.names)
					} else {
						err = chaos.StopByPattern(client, cmd.pattern)
					}
				case "KILL":
					if pattern == "" {
						err = chaos.KillByName(client, cmd.names, cmd.signal)
					} else {
						err = chaos.KillByPattern(client, cmd.pattern, cmd.signal)
					}
				case "RM":
					if pattern == "" {
						err = chaos.RemoveByName(client, cmd.names, true)
					} else {
						err = chaos.RemoveByPattern(client, cmd.pattern, true)
					}
				}
				if err != nil {
					log.Error(err)
				}
			}(cmd)
		}
	}
	return nil
}

func handleSignals() {
	// Graceful shut-down on SIGINT/SIGTERM
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)

	go func() {
		<-c
		wg.Wait()
		os.Exit(1)
	}()
}

// tlsConfig translates the command-line options into a tls.Config struct
func tlsConfig(c *cli.Context) (*tls.Config, error) {
	var tlsConfig *tls.Config
	var err error
	caCertFlag := c.GlobalString("tlscacert")
	certFlag := c.GlobalString("tlscert")
	keyFlag := c.GlobalString("tlskey")

	if c.GlobalBool("tls") || c.GlobalBool("tlsverify") {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: !c.GlobalBool("tlsverify"),
		}

		// Load CA cert
		if caCertFlag != "" {
			var caCert []byte

			if strings.HasPrefix(caCertFlag, "/") {
				caCert, err = ioutil.ReadFile(caCertFlag)
				if err != nil {
					return nil, err
				}
			} else {
				caCert = []byte(caCertFlag)
			}

			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)

			tlsConfig.RootCAs = caCertPool
		}

		// Load client certificate
		if certFlag != "" && keyFlag != "" {
			var cert tls.Certificate

			if strings.HasPrefix(certFlag, "/") && strings.HasPrefix(keyFlag, "/") {
				cert, err = tls.LoadX509KeyPair(certFlag, keyFlag)
				if err != nil {
					return nil, err
				}
			} else {
				cert, err = tls.X509KeyPair([]byte(certFlag), []byte(keyFlag))
				if err != nil {
					return nil, err
				}
			}
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
	}

	return tlsConfig, nil
}
