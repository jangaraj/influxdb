package export

import (
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/influxdata/influxdb/cmd/influx-tools/internal/format"
	"github.com/influxdata/influxdb/cmd/influx-tools/internal/storage"
	"github.com/influxdata/influxdb/cmd/influx-tools/server"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/services/meta"
	"github.com/influxdata/influxdb/tsdb"
)

type ExporterConfig struct {
	Database      string
	RP            string
	ShardDuration time.Duration
}

type Exporter struct {
	MetaClient server.MetaClient
	TSDBStore  *tsdb.Store
	store      *storage.Store

	db, rp       string
	d            time.Duration
	sourceGroups []meta.ShardGroupInfo
	targetGroups []meta.ShardGroupInfo

	// source data time range
	startDate time.Time
	endDate   time.Time
}

func NewExporter(server server.Interface, cfg *ExporterConfig) (*Exporter, error) {
	client := server.MetaClient()

	dbi := client.Database(cfg.Database)
	if dbi == nil {
		return nil, fmt.Errorf("database '%s' does not exist", cfg.Database)
	}

	if cfg.RP == "" {
		// select default RP
		cfg.RP = dbi.DefaultRetentionPolicy
	}

	rpi, err := client.RetentionPolicy(cfg.Database, cfg.RP)
	if err != nil {
		return nil, fmt.Errorf("retention policy '%s' does not exist", cfg.RP)
	}

	if rpi == nil {
		return nil, fmt.Errorf("retention policy '%s' does not exist", cfg.RP)
	}

	store := tsdb.NewStore(server.TSDBConfig().Dir)
	store.WithLogger(server.Logger())
	store.EngineOptions.Config = server.TSDBConfig()
	store.EngineOptions.EngineVersion = server.TSDBConfig().Engine
	store.EngineOptions.IndexVersion = server.TSDBConfig().Index
	store.EngineOptions.DatabaseFilter = func(database string) bool {
		return database == cfg.Database
	}
	store.EngineOptions.RetentionPolicyFilter = func(_, rp string) bool {
		return rp == cfg.RP
	}
	store.EngineOptions.ShardFilter = func(_, _ string, _ uint64) bool {
		return false
	}

	return &Exporter{
		MetaClient: client,
		TSDBStore:  store,
		store:      &storage.Store{TSDBStore: store},
		db:         cfg.Database,
		rp:         cfg.RP,
		d:          cfg.ShardDuration,
	}, nil
}

func (e *Exporter) Open() (err error) {
	err = e.TSDBStore.Open()
	if err != nil {
		return err
	}

	err = e.loadShardGroups()
	if err != nil {
		return err
	}

	e.targetGroups = PlanShardGroups(e.sourceGroups, e.startDate, e.endDate, e.d)

	return nil
}

func (e *Exporter) PrintPlan(w io.Writer) {
	fmt.Fprintf(w, "Source data from: %s -> %s\n\n", e.startDate, e.endDate)
	fmt.Fprintf(w, "Converting source from %d shard group(s) to %d shard groups\n\n", len(e.sourceGroups), len(e.targetGroups))
	printShardGroups(w, e.sourceGroups)
	fmt.Fprintln(w)
	printShardGroups(w, e.targetGroups)
}

func printShardGroups(w io.Writer, target []meta.ShardGroupInfo) {
	tw := tabwriter.NewWriter(w, 25, 8, 1, '\t', 0)
	fmt.Fprintln(tw, "ID\tStart\tEnd")
	for i := 0; i < len(target); i++ {
		g := target[i]
		fmt.Fprintf(tw, "%d\t%s\t%s\n", g.ID, g.StartTime, g.EndTime)
	}
	tw.Flush()
}

func (e *Exporter) SourceTimeRange() (time.Time, time.Time)  { return e.startDate, e.endDate }
func (e *Exporter) SourceShardGroups() []meta.ShardGroupInfo { return e.sourceGroups }
func (e *Exporter) TargetShardGroups() []meta.ShardGroupInfo { return e.targetGroups }

func (e *Exporter) loadShardGroups() error {
	min := time.Unix(0, models.MinNanoTime)
	max := time.Unix(0, models.MaxNanoTime)

	// TODO(sgc): verify Enterprise MetaClient wrapper limits to shard groups owned by this node only
	groups, err := e.MetaClient.ShardGroupsByTimeRange(e.db, e.rp, min, max)
	if err != nil {
		return err
	}

	if len(groups) == 0 {
		return nil
	}

	sort.Sort(meta.ShardGroupInfos(groups))
	e.sourceGroups = groups
	e.startDate = groups[0].StartTime
	e.endDate = groups[len(groups)-1].EndTime

	return nil
}

func (e *Exporter) shardsGroupsByTimeRange(min, max time.Time) []meta.ShardGroupInfo {
	groups := make([]meta.ShardGroupInfo, 0, len(e.sourceGroups))
	for _, g := range e.sourceGroups {
		if !g.Overlaps(min, max) {
			continue
		}
		groups = append(groups, g)
	}
	return groups
}

func (e *Exporter) WriteTo(w format.Writer) error {
	for _, g := range e.targetGroups {
		min, max := g.StartTime, g.EndTime
		rs, err := e.read(min, max.Add(-1))
		if err != nil || rs == nil {
			return err
		}

		format.WriteBucket(w, min.UnixNano(), max.UnixNano(), rs)
		rs.Close()
	}
	return nil
}

// Read creates a ResultSet that reads all points with a timestamp ts, such that start ≤ ts < end.
func (e *Exporter) read(min, max time.Time) (*storage.ResultSet, error) {
	shards, err := e.getShards(min, max)
	if err != nil {
		return nil, err
	}

	req := storage.ReadRequest{
		Database:    e.db,
		RP:          e.rp,
		Shards:      shards,
		Start:       min.UnixNano(),
		End:         max.UnixNano(),
		PointsLimit: math.MaxUint64,
	}

	return e.store.Read(context.Background(), &req)
}

func (e *Exporter) Close() error {
	return e.TSDBStore.Close()
}

func (e *Exporter) getShards(min, max time.Time) ([]*tsdb.Shard, error) {
	groups := e.shardsGroupsByTimeRange(min, max)
	var ids []uint64
	for _, g := range groups {
		for _, s := range g.Shards {
			ids = append(ids, s.ID)
		}
	}

	shards := e.TSDBStore.Shards(ids)
	if len(shards) == len(ids) {
		return shards, nil
	}

	return e.openStoreWithShardsIDs(ids)
}

func (e *Exporter) openStoreWithShardsIDs(ids []uint64) ([]*tsdb.Shard, error) {
	e.TSDBStore.Close()
	e.TSDBStore.EngineOptions.ShardFilter = func(_, _ string, id uint64) bool {
		for i := range ids {
			if id == ids[i] {
				return true
			}
		}
		return false
	}
	if err := e.TSDBStore.Open(); err != nil {
		return nil, err
	}
	return e.TSDBStore.Shards(ids), nil
}
