package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	_ "github.com/go-sql-driver/mysql" // mysql driver
	"github.com/google/uuid"
	"github.com/pingcap/errors"
	pd "github.com/pingcap/pd/client"
	"github.com/pingcap/ticdc/cdc/kv"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/store/tikv/oracle"
	"github.com/spf13/cobra"
	"go.etcd.io/etcd/clientv3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
)

func init() {
	rootCmd.AddCommand(cliCmd)

	cliCmd.Flags().StringVar(&pdAddress, "pd-addr", "localhost:2379", "address of PD")
	cliCmd.Flags().Uint64Var(&startTs, "start-ts", 0, "start ts of changefeed")
	cliCmd.Flags().Uint64Var(&targetTs, "target-ts", 0, "target ts of changefeed")
	cliCmd.Flags().StringVar(&sinkURI, "sink-uri", "root@tcp(127.0.0.1:3306)/", "sink uri")
	cliCmd.Flags().StringVar(&configFile, "config", "", "path of the configuration file")
	cliCmd.Flags().StringSliceVar(&opts, "opts", nil, "in key=value format")
}

var (
	opts       []string
	pdAddress  string
	startTs    uint64
	targetTs   uint64
	sinkURI    string
	configFile string
)

var cliCmd = &cobra.Command{
	Use:   "cli",
	Short: "simulate client to create changefeed",
	Long:  ``,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		etcdCli, err := clientv3.New(clientv3.Config{
			Endpoints:   []string{pdAddress},
			DialTimeout: 5 * time.Second,
			DialOptions: []grpc.DialOption{
				grpc.WithConnectParams(grpc.ConnectParams{
					Backoff: backoff.Config{
						BaseDelay:  time.Second,
						Multiplier: 1.1,
						Jitter:     0.1,
						MaxDelay:   3 * time.Second,
					},
					MinConnectTimeout: 3 * time.Second,
				}),
			},
		})
		if err != nil {
			return err
		}
		cli := kv.NewCDCEtcdClient(etcdCli)
		pdCli, err := pd.NewClient([]string{pdAddress}, pd.SecurityOption{})
		if err != nil {
			return err
		}
		id := uuid.New().String()
		if startTs == 0 {
			ts, logical, err := pdCli.GetTS(ctx)
			if err != nil {
				return err
			}
			startTs = oracle.ComposeTS(ts, logical)
		}
		err = verifyStartTs(ctx, startTs, cli)
		if err != nil {
			return err
		}

		cfg := new(model.ReplicaConfig)
		if len(configFile) > 0 {
			if err := strictDecodeFile(configFile, "cdc", cfg); err != nil {
				return err
			}
		}

		detail := &model.ChangeFeedInfo{
			SinkURI:    sinkURI,
			Opts:       make(map[string]string),
			CreateTime: time.Now(),
			StartTs:    startTs,
			TargetTs:   targetTs,
			Config:     cfg,
		}

		for _, opt := range opts {
			s := strings.Split(opt, "=")
			if len(s) <= 0 || len(s) > 2 {
				fmt.Printf("omit opt: %s", opt)
			}

			var key string
			var value string

			key = s[0]
			if len(s) > 1 {
				value = s[1]
			}
			detail.Opts[key] = value
		}

		d, err := detail.Marshal()
		if err != nil {
			return err
		}
		fmt.Printf("create changefeed ID: %s detail %s\n", id, d)
		return cli.SaveChangeFeedInfo(ctx, detail, id)
	},
}

func verifyStartTs(ctx context.Context, startTs uint64, cli kv.CDCEtcdClient) error {
	resp, err := cli.Client.Get(ctx, tikv.GcSavedSafePoint)
	if err != nil {
		return errors.Trace(err)
	}
	if resp.Count == 0 {
		return nil
	}
	safePoint, err := strconv.ParseUint(string(resp.Kvs[0].Value), 10, 64)
	if err != nil {
		return errors.Trace(err)
	}
	if startTs < safePoint {
		return errors.Errorf("startTs %d less than gcSafePoint %d", startTs, safePoint)
	}
	return nil
}

// strictDecodeFile decodes the toml file strictly. If any item in confFile file is not mapped
// into the Config struct, issue an error and stop the server from starting.
func strictDecodeFile(path, component string, cfg interface{}) error {
	metaData, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return errors.Trace(err)
	}

	if undecoded := metaData.Undecoded(); len(undecoded) > 0 {
		var b strings.Builder
		for i, item := range undecoded {
			if i != 0 {
				b.WriteString(", ")
			}
			b.WriteString(item.String())
		}
		err = errors.Errorf("component %s's config file %s contained unknown configuration options: %s",
			component, path, b.String())
	}

	return errors.Trace(err)
}
