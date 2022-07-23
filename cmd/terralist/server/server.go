package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"terralist/internal/server"
	"terralist/pkg/auth"
	authFactory "terralist/pkg/auth/factory"
	"terralist/pkg/auth/github"
	"terralist/pkg/cli"
	"terralist/pkg/database"
	dbFactory "terralist/pkg/database/factory"
	"terralist/pkg/database/postgresql"
	"terralist/pkg/database/sqlite"
	"terralist/pkg/storage/resolver"
	storageFactory "terralist/pkg/storage/resolver/factory"
	"terralist/pkg/storage/resolver/local"
	"terralist/pkg/storage/resolver/proxy"
	"terralist/pkg/storage/resolver/s3"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Command is an abstraction for the server command
type Command struct {
	ServerCreator Creator
	Viper         *viper.Viper

	RunningMode string

	SilenceOutput bool
}

// Creator creates the server
type Creator interface {
	NewServer(userConfig server.UserConfig, config server.Config) (Starter, error)
}

// DefaultCreator is the concrete implementation of Creator
type DefaultCreator struct{}

// Starter starts the server
type Starter interface {
	Start() error
}

// NewServer returns the real server object
func (d *DefaultCreator) NewServer(userConfig server.UserConfig, config server.Config) (Starter, error) {
	return server.NewServer(userConfig, config)
}

func (s *Command) Init() *cobra.Command {
	c := &cobra.Command{
		Use:           "server",
		Short:         "Starts the Terralist server",
		Long:          "Starts the Terralist RESTful server.",
		SilenceErrors: true,
		SilenceUsage:  true,
		PreRunE: s.withErrPrint(func(cmd *cobra.Command, args []string) error {
			return s.preRun()
		}),
		RunE: s.withErrPrint(func(cmd *cobra.Command, args []string) error {
			return s.run()
		}),
	}

	// Configure viper to accept env vars with prefix instead of flags
	s.Viper.SetEnvPrefix("TERRALIST")
	s.Viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	s.Viper.AutomaticEnv()
	s.Viper.SetTypeByDefaultValue(true)

	c.SetUsageTemplate(cli.UsageTmpl(flags))
	// In case of invalid flags, print the error
	c.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		s.printErr(err)
		return err
	})

	for name, f := range flags {
		usage := f.Format() + "\n"

		if fg, ok := f.(*cli.StringFlag); ok {
			c.Flags().String(name, fg.DefaultValue, usage)
		} else if fg, ok := f.(*cli.IntFlag); ok {
			c.Flags().Int(name, fg.DefaultValue, usage)
		} else if fg, ok := f.(*cli.BoolFlag); ok {
			c.Flags().Bool(name, fg.DefaultValue, usage)
		}

		if f.IsHidden() {
			_ = c.Flags().MarkHidden(name)
		}

		_ = s.Viper.BindPFlag(name, c.Flags().Lookup(name))
	}

	return c
}

func (s *Command) preRun() error {
	// If passed a config file then try and load it.
	configFile := s.Viper.GetString(ConfigFlag)

	if configFile != "" {
		s.Viper.SetConfigFile(configFile)
		if err := s.Viper.ReadInConfig(); err != nil {
			return errors.Wrapf(err, "invalid config: reading %s", configFile)
		}
	}

	return nil
}

func (s *Command) run() error {
	var raw map[string]any

	if err := s.Viper.Unmarshal(&raw); err != nil {
		return err
	}

	// Set values from viper
	for k, v := range raw {
		if _, ok := flags[k]; ok {
			// If it's not set, set the default value
			if !s.Viper.IsSet(k) {
				_ = flags[k].Set(nil)

				continue
			}

			if err := flags[k].Set(v); err != nil {
				return fmt.Errorf("could not unpack flags: %v", err)
			}
		}
	}

	// Validate flag values
	for k, v := range flags {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("could not validate %v: %v", k, err)
		}
	}

	userConfig := server.UserConfig{
		LogLevel:           flags[LogLevelFlag].(*cli.StringFlag).Value,
		Port:               flags[PortFlag].(*cli.IntFlag).Value,
		TokenSigningSecret: flags[TokenSigningSecretFlag].(*cli.StringFlag).Value,
	}

	if s.RunningMode == "debug" {
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	} else {
		switch userConfig.LogLevel {
		case "trace":
			zerolog.SetGlobalLevel(zerolog.TraceLevel)
		case "debug":
			zerolog.SetGlobalLevel(zerolog.DebugLevel)
		case "info":
			zerolog.SetGlobalLevel(zerolog.InfoLevel)
		case "warn":
			zerolog.SetGlobalLevel(zerolog.WarnLevel)
		case "error":
			zerolog.SetGlobalLevel(zerolog.ErrorLevel)
		}
	}

	// Initialize database
	var db database.Engine
	var err error
	switch flags[DatabaseBackendFlag].(*cli.StringFlag).Value {
	case "sqlite":
		db, err = dbFactory.NewDatabase(database.SQLITE, &sqlite.Config{
			Path: flags[SQLitePathFlag].(*cli.StringFlag).Value,
		})
	case "postgresql":
		db, err = dbFactory.NewDatabase(database.POSTGRESQL, &postgresql.Config{
			URL:      flags[PostgreSQLURLFlag].(*cli.StringFlag).Value,
			Username: flags[PostgreSQLUsernameFlag].(*cli.StringFlag).Value,
			Password: flags[PostgreSQLPasswordFlag].(*cli.StringFlag).Value,
			Hostname: flags[PostgreSQLHostFlag].(*cli.StringFlag).Value,
			Port:     flags[PostgreSQLPortFlag].(*cli.IntFlag).Value,
			Name:     flags[PostgreSQLDatabaseFlag].(*cli.StringFlag).Value,
		})
	}
	if err != nil {
		return err
	}

	// Initialize Auth provider
	var provider auth.Provider
	switch flags[OAuthProviderFlag].(*cli.StringFlag).Value {
	case "github":
		provider, err = authFactory.NewProvider(auth.GITHUB, &github.Config{
			ClientID:     flags[GitHubClientIDFlag].(*cli.StringFlag).Value,
			ClientSecret: flags[GitHubClientSecretFlag].(*cli.StringFlag).Value,
			Organization: flags[GitHubOrganizationFlag].(*cli.StringFlag).Value,
		})
	}
	if err != nil {
		return err
	}

	// Initialize home directory
	homeDirClean := filepath.Clean(flags[HomeDirectoryFlag].(*cli.StringFlag).Value)
	if strings.HasPrefix(homeDirClean, "~") {
		userHomeDir, _ := os.UserHomeDir()
		homeDirClean = fmt.Sprintf("%s%s", userHomeDir, homeDirClean[1:])
	}

	homeDir, err := filepath.Abs(homeDirClean)
	if err != nil {
		return fmt.Errorf("invalid value for home directory: %v", err)
	}

	// Make sure Home Directory exists
	if err := os.MkdirAll(homeDir, os.ModePerm); err != nil {
		return fmt.Errorf("could not create the home directory: %v", err)
	}

	// Initialize storage resolver
	var res resolver.Resolver
	switch flags[StorageResolverFlag].(*cli.StringFlag).Value {
	case "proxy":
		res, err = storageFactory.NewResolver(resolver.PROXY, &proxy.Config{})
	case "local":
		res, err = storageFactory.NewResolver(resolver.LOCAL, &local.Config{
			HomeDirectory: homeDir,
		})
	case "s3":
		res, err = storageFactory.NewResolver(resolver.S3, &s3.Config{
			HomeDirectory:   homeDir,
			BucketName:      flags[S3BucketNameFlag].(*cli.StringFlag).Value,
			BucketRegion:    flags[S3BucketRegionFlag].(*cli.StringFlag).Value,
			AccessKeyID:     flags[S3AccessKeyIDFlag].(*cli.StringFlag).Value,
			SecretAccessKey: flags[S3SecretAccessKeyFlag].(*cli.StringFlag).Value,
			LinkExpire:      flags[S3PresignExpireFlag].(*cli.IntFlag).Value,
		})
	}
	if err != nil {
		return err
	}

	srv, err := s.ServerCreator.NewServer(userConfig, server.Config{
		Database:    db,
		Provider:    provider,
		Resolver:    res,
		RunningMode: s.RunningMode,
	})

	if err != nil {
		return errors.Wrap(err, "initializing server")
	}

	return srv.Start()
}

// withErrPrint prints out any cmd errors to stderr
func (s *Command) withErrPrint(f func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := f(cmd, args)
		if err != nil && !s.SilenceOutput {
			s.printErr(err)
		}
		return err
	}
}

// printErr prints err to stderr using a red terminal color
func (s *Command) printErr(err error) {
	log.Error().AnErr("error", err).Send()
}