package command

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/zeromake/docker-debug/internal/config"
	"github.com/zeromake/docker-debug/pkg/tty"
)

var rootCmd = newExecCommand()

type execOptions struct {
	host         string
	image        string
	detachKeys   string
	user         string
	privileged   bool
	workDir      string
	targetDir    string
	container    string
	certDir      string
	command      []string
	name         string
	volumes      []string
	ipc          bool
	securityOpts []string
	capAdds      []string
}

func newExecOptions() execOptions {
	return execOptions{}
}

func newExecCommand() *cobra.Command {
	options := newExecOptions()

	cmd := &cobra.Command{
		Use:   "docker-debug [OPTIONS] CONTAINER COMMAND [ARG...]",
		Short: "Run a command in a running container",
		Args:  RequiresMinArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			options.container = args[0]
			options.command = args[1:]
			err := runExec(options)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}

	flags := cmd.Flags()
	flags.SetInterspersed(false)

	flags.StringArrayVarP(&options.volumes, "volume", "v", nil, "Attach a filesystem mount to the container")
	flags.StringVarP(&options.image, "image", "i", "", "use this image")
	flags.StringVarP(&options.name, "name", "n", "", "docker config name")
	flags.StringVarP(&options.host, "host", "H", "", "connection host's docker (format: tcp://192.168.99.100:2376)")
	flags.StringVarP(&options.certDir, "cert-dir", "c", "", "cert dir use tls")
	flags.StringVarP(&options.detachKeys, "detach-keys", "d", "", "Override the key sequence for detaching a container")
	flags.StringVarP(&options.user, "user", "u", "", "Username or UID (format: <name|uid>[:<group|gid>])")
	flags.BoolVarP(&options.privileged, "privileged", "p", false, "Give extended privileges to the command")
	flags.StringVarP(&options.workDir, "work-dir", "w", "", "Working directory inside the container")
	_ = flags.SetAnnotation("work-dir", "version", []string{"1.35"})
	flags.StringVarP(&options.targetDir, "target-dir", "t", "", "Working directory inside the container")
	flags.StringArrayVarP(&options.securityOpts, "security-opts", "s", nil, "Add security options to the Docker container")
	flags.StringArrayVarP(&options.capAdds, "cap-adds", "C", nil, "Add Linux capabilities to the Docker container")
	flags.BoolVar(&options.ipc, "ipc", false, "share target container ipc")
	return cmd
}

func buildCli(ctx context.Context, options execOptions) (*DebugCli, error) {
	conf, err := config.LoadConfig()
	if err != nil {
		return nil, err
	}
	opts := []DebugCliOption{
		WithConfig(conf),
	}
	if options.image != "" {
		conf.Image = options.image
	}
	if conf.Image == "" {
		return nil, errors.New("not set image")
	}
	if options.host != "" {
		dockerConfig := &config.DockerConfig{
			Host: options.host,
		}
		if options.certDir != "" {
			dockerConfig.TLS = true
			dockerConfig.CertDir = options.certDir
		}
		opts = append(opts, WithClientConfig(dockerConfig))
	} else {
		name := conf.DockerConfigDefault
		opt, ok := conf.DockerConfig[conf.DockerConfigDefault]
		if options.name != "" {
			name = options.name
			opt, ok = conf.DockerConfig[options.name]
		}
		if !ok {
			return nil, errors.Errorf("not find %s docker config", name)
		}
		opts = append(opts, WithClientConfig(opt))
	}

	cli, err := NewDebugCli(opts...)
	if err != nil {
		return nil, err
	}
	cli.ctx = ctx
	return cli, err
}

func runExec(options execOptions) error {
	var ctx, cancel = context.WithCancel(context.Background())
	logrus.SetLevel(logrus.ErrorLevel)
	var containerID string
	cli, err := buildCli(ctx, options)
	if err != nil {
		cancel()
		return err
	}
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		<-c
		cancel()
	}()
	conf := cli.Config()
	defer func() {
		if err != nil {
			if errors.Is(err, context.Canceled) {
				cli.ctx = context.Background()
			}
		}
		if containerID != "" {
			err = cli.ContainerClean(containerID)
			if err != nil {
				logrus.Debugf("%+v", err)
			}
		}
		err = cli.Close()
		if err != nil {
			logrus.Debugf("%+v", err)
		}
	}()
	_, err = cli.Ping()
	if err != nil {
		cancel()
		return err
	}
	// find image
	images, err := cli.FindImage(conf.Image)
	if err != nil {
		cancel()
		return err
	}

	if len(images) == 0 {
		// pull image
		err = cli.PullImage(conf.Image)
		if err != nil {
			cancel()
			return err
		}
	}
	containerID, err = cli.FindContainer(options.container)
	if err != nil {
		cancel()
		return err
	}
	containerID, err = cli.CreateContainer(containerID, options)
	if err != nil {
		cancel()
		return err
	}
	resp, err := cli.ExecCreate(options, containerID)
	if err != nil {
		cancel()
		return err
	}

	errCh := make(chan error, 1)

	go func() {
		defer close(errCh)
		errCh <- func() error {
			return cli.ExecStart(options, resp.ID)
		}()
	}()
	if cli.In().IsTerminal() {
		if err := tty.MonitorTtySize(ctx, cli.Client(), cli.Out(), resp.ID, true); err != nil {
			_, _ = fmt.Fprintln(cli.Err(), "Error monitoring TTY size:", err)
		}
	}

	err = <-errCh
	if err != nil {
		logrus.Debugf("Error hijack: %s", err)
		cancel()
		return err
	}
	cancel()
	return nil
}

// Execute main func
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		logrus.Debugf("%+v", err)
	}
}
