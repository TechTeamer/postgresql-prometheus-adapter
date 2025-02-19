package postgresql

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
)

type tMetricIDMap map[string]int64

// Config for the database
type Config struct {
	CommitSecs      int
	CommitRows      int
	PGWriters       int
	PGParsers       int
	PartitionScheme string
}

var promSamples = list.New()

// QueueMutex is used thread safe operations on promSamples list object.
var QueueMutex sync.Mutex

// PGWriter - Threaded writer
type PGWriter struct {
	DB          *pgxpool.Pool
	id          int
	KeepRunning bool
	Running     bool

	valueRows [][]interface{}

	PGWriterMutex sync.Mutex
	logger        log.Logger
}

// PGParser - Threaded parser
type PGParser struct {
	id          int
	KeepRunning bool
	Running     bool

	lastPartitionTS time.Time
	valueRows       [][]interface{}
}

// RunPGParser starts the client and listens for a shutdown call.
func (p *PGParser) RunPGParser(tid int, partitionScheme string, c *PGWriter) {
	var samples *model.Samples
	p.id = tid
	level.Info(c.logger).Log(fmt.Sprintf("bgparser%d", p.id), "Started")
	p.Running = true
	p.KeepRunning = true

	// Loop that runs forever
	for p.KeepRunning {
		samples = Pop()
		if samples != nil {
			for _, sample := range *samples {
				sMetric := metricString(sample.Metric)
				ts := time.Unix(sample.Timestamp.Unix(), 0)
				milliseconds := sample.Timestamp.UnixNano() / 1000000

				i := strings.Index(sMetric, "{")
				jsonbMap := make(map[string]interface{})
				json.Unmarshal([]byte(sMetric[i:]), &jsonbMap)

				c.PGWriterMutex.Lock()
				c.valueRows = append(c.valueRows, []interface{}{toTimestamp(milliseconds), sMetric[:i], float64(sample.Value), jsonbMap})
				c.PGWriterMutex.Unlock()

				if ts.Year() != p.lastPartitionTS.Year() ||
					ts.Month() != p.lastPartitionTS.Month() ||
					ts.Day() != p.lastPartitionTS.Day() {
					p.lastPartitionTS = ts
					_ = c.setupPgPartitions(partitionScheme, p.lastPartitionTS)
				}
			}
			runtime.GC()
		}
		time.Sleep(10 * time.Millisecond)
	}
	level.Info(c.logger).Log(fmt.Sprintf("bgparser%d", p.id), "Shutdown")
	p.Running = false
}

// PGParserShutdown is a graceful shutdown
func (p *PGParser) PGParserShutdown() {
	p.KeepRunning = false
}

// RunPGWriter starts the client and listens for a shutdown call.
func (c *PGWriter) RunPGWriter(l log.Logger, tid int, commitSecs int, commitRows int, Parsers int, partitionScheme string) {
	c.logger = l
	c.id = tid
	period := commitSecs * 1000
	var err error
	var parser [20]PGParser

	c.DB, err = pgxpool.Connect(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: Unable to connect to database using DATABASE_URL=", os.Getenv("DATABASE_URL"))
		os.Exit(1)
	}

	if c.id == 0 {
		c.setupPgPrometheus()
		_ = c.setupPgPartitions(partitionScheme, time.Now())
	}
	level.Info(c.logger).Log(fmt.Sprintf("bgwriter%d", c.id), fmt.Sprintf("Starting %d Parsers", Parsers))
	for p := 0; p < Parsers; p++ {
		go parser[p].RunPGParser(p, partitionScheme, c)
		defer parser[p].PGParserShutdown()
	}
	level.Info(c.logger).Log(fmt.Sprintf("bgwriter%d", c.id), "Started")
	c.Running = true
	c.KeepRunning = true
	// Loop that runs forever
	for c.KeepRunning {
		if (period <= 0 && len(c.valueRows) > 0) || (len(c.valueRows) > commitRows) {
			c.PGWriterSave()
			period = commitSecs * 1000
		} else {
			time.Sleep(10 * time.Millisecond)
			period -= 10
		}
	}
	c.PGWriterSave()
	level.Info(c.logger).Log(fmt.Sprintf("bgwriter%d", c.id), "Shutdown")
	c.Running = false
}

// PGWriterShutdown - Set shutdown flag for graceful shutdown
func (c *PGWriter) PGWriterShutdown() {
	c.KeepRunning = false
}

// PGWriterSave save data to DB
func (c *PGWriter) PGWriterSave() {
	var err error
	begin := time.Now()
	c.PGWriterMutex.Lock()
	rowCount := int64(len(c.valueRows))
	copyCount, err := c.DB.CopyFrom(context.Background(), pgx.Identifier{"metrics"}, []string{"time", "name", "value", "labels"}, pgx.CopyFromRows(c.valueRows))
	c.valueRows = nil
	c.PGWriterMutex.Unlock()

	if err != nil {
		level.Error(c.logger).Log("msg", "COPY failed for metrics", "err", err)
	}
	if copyCount != rowCount {
		level.Error(c.logger).Log("msg", "All rows not copied metrics", "copyCount", copyCount, "rowCount", rowCount)
	}

	duration := time.Since(begin).Seconds()
	level.Info(c.logger).Log("metric", fmt.Sprintf("BGWriter%d: Processed samples count,%d, duration,%v", c.id, rowCount, duration))
}

// Push - Push element at then end of list
func Push(samples *model.Samples) {
	QueueMutex.Lock()
	promSamples.PushBack(samples)
	QueueMutex.Unlock()
}

// Pop - Pop first element from list
func Pop() *model.Samples {
	QueueMutex.Lock()
	defer QueueMutex.Unlock()
	p := promSamples.Front()
	if p != nil {
		return promSamples.Remove(p).(*model.Samples)
	}
	return nil
}

// Client - struct to hold critical values
type Client struct {
	logger log.Logger
	DB     *pgxpool.Pool
	cfg    *Config
}

// NewClient creates a new PostgreSQL client
func NewClient(logger log.Logger, cfg *Config) *Client {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	pool, err := pgxpool.Connect(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: Unable to connect to database using DATABASE_URL=", os.Getenv("DATABASE_URL"))
		os.Exit(1)
	}

	client := &Client{
		logger: logger,
		DB:     pool,
		cfg:    cfg,
	}

	return client
}

func (c *PGWriter) setupPgPrometheus() error {
	level.Info(c.logger).Log("msg", "creating tables")

	_, err := c.DB.Exec(context.Background(), "CREATE TABLE IF NOT EXISTS metrics ( time timestamptz, name TEXT NOT NULL, value FLOAT8, labels jsonb, UNIQUE(time, name, labels) ) PARTITION BY RANGE (time)")
	if err != nil {
		return err
	}

	_, err = c.DB.Exec(context.Background(), "CREATE INDEX IF NOT EXISTS metrics_time_brin_idx ON metrics USING BRIN (time)")
	if err != nil {
		return err
	}

	_, err = c.DB.Exec(context.Background(), "CREATE INDEX IF NOT EXISTS metrics_name_time_idx on metrics USING btree (name, time DESC)")
	if err != nil {
		return err
	}

	return nil
}

func (c *PGWriter) setupPgPartitions(partitionScheme string, lastPartitionTS time.Time) error {
	sDate := lastPartitionTS
	eDate := sDate
	if partitionScheme == "daily" {
		level.Info(c.logger).Log("msg", "Creating partition, daily")
		_, err := c.DB.Exec(context.Background(), fmt.Sprintf("CREATE TABLE IF NOT EXISTS metrics_%s PARTITION OF metrics FOR VALUES FROM ('%s 00:00:00') TO ('%s 00:00:00')", sDate.Format("20060102"), sDate.Format("2006-01-02"), eDate.AddDate(0, 0, 1).Format("2006-01-02")))
		if err != nil {
			return err
		}
	} else if partitionScheme == "hourly" {
		sql := fmt.Sprintf("CREATE TABLE IF NOT EXISTS metrics_%s PARTITION OF metrics FOR VALUES FROM ('%s 00:00:00') TO ('%s 00:00:00') PARTITION BY RANGE (time);", sDate.Format("20060102"), sDate.Format("2006-01-02"), eDate.AddDate(0, 0, 1).Format("2006-01-02"))
		var h int
		for h = 0; h < 23; h++ {
			sql = fmt.Sprintf("%s CREATE TABLE IF NOT EXISTS metrics_%s_%02d PARTITION OF metrics_%s FOR VALUES FROM ('%s %02d:00:00') TO ('%s %02d:00:00');", sql, sDate.Format("20060102"), h, sDate.Format("20060102"), sDate.Format("2006-01-02"), h, eDate.Format("2006-01-02"), h+1)
		}
		level.Info(c.logger).Log("msg", "Creating partition, hourly")
		_, err := c.DB.Exec(context.Background(), fmt.Sprintf("%s CREATE TABLE IF NOT EXISTS metrics_%s_%02d PARTITION OF metrics_%s FOR VALUES FROM ('%s %02d:00:00') TO ('%s 00:00:00');", sql, sDate.Format("20060102"), h, sDate.Format("20060102"), sDate.Format("2006-01-02"), h, eDate.AddDate(0, 0, 1).Format("2006-01-02")))
		if err != nil {
			return err
		}
	}
	return nil
}

func metricString(m model.Metric) string {
	metricName, hasName := m[model.MetricNameLabel]
	numLabels := len(m) - 1
	if !hasName {
		numLabels = len(m)
	}
	labelStrings := make([]string, 0, numLabels)
	for label, value := range m {
		if label != model.MetricNameLabel {
			labelStrings = append(labelStrings, fmt.Sprintf("\"%s\": %q", label, value))
		}
	}

	switch numLabels {
	case 0:
		if hasName {
			return string(metricName)
		}
		return "{}"
	default:
		sort.Strings(labelStrings)
		return fmt.Sprintf("%s{%s}", metricName, strings.Join(labelStrings, ", "))
	}
}

// Write implements the Writer interface and writes metric samples to the database
func (c *Client) Write(samples model.Samples) error {
	Push(&samples)
	return nil
}

type sampleLabels struct {
	JSON        []byte
	Map         map[string]string
	OrderedKeys []string
}

func createOrderedKeys(m *map[string]string) []string {
	keys := make([]string, 0, len(*m))
	for k := range *m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Close - Close database connections
func (c *Client) Close() {
	if c.DB != nil {
		c.DB.Close()
	}
}

func (l *sampleLabels) Scan(value interface{}) error {
	if value == nil {
		l = &sampleLabels{}
		return nil
	}

	switch t := value.(type) {
	case []uint8:
		m := make(map[string]string)
		err := json.Unmarshal(t, &m)

		if err != nil {
			return err
		}

		*l = sampleLabels{
			JSON:        t,
			Map:         m,
			OrderedKeys: createOrderedKeys(&m),
		}
		return nil
	}
	return fmt.Errorf("invalid labels value %s", reflect.TypeOf(value))
}

func (l sampleLabels) String() string {
	return string(l.JSON)
}

func (l sampleLabels) key(extra string) string {
	// 0xff cannot cannot occur in valid UTF-8 sequences, so use it
	// as a separator here.
	separator := "\xff"
	pairs := make([]string, 0, len(l.Map)+1)
	pairs = append(pairs, extra+separator)

	for _, k := range l.OrderedKeys {
		pairs = append(pairs, k+separator+l.Map[k])
	}
	return strings.Join(pairs, separator)
}

func (l *sampleLabels) len() int {
	return len(l.OrderedKeys)
}

// Read implements the Reader interface and reads metrics samples from the database
func (c *Client) Read(req *prompb.ReadRequest) (*prompb.ReadResponse, error) {

	fmt.Printf("READ req.Queries: %v\n", req.Queries)
	labelsToSeries := map[string]*prompb.TimeSeries{}

	for _, q := range req.Queries {
		command, err := c.buildCommand(q)

		if err != nil {
			return nil, err
		}

		level.Debug(c.logger).Log("msg", "Executed query", "query", command)

		rows, err := c.DB.Query(context.Background(), command)

		if err != nil {
			rows.Close()
			return nil, err
		}

		for rows.Next() {
			var (
				value  float64
				name   string
				labels sampleLabels
				time   time.Time
			)
			err := rows.Scan(&time, &name, &value, &labels)

			if err != nil {
				rows.Close()
				return nil, err
			}

			key := labels.key(name)
			ts, ok := labelsToSeries[key]

			if !ok {
				labelPairs := make([]prompb.Label, 0, labels.len()+1)
				labelPairs = append(labelPairs, prompb.Label{
					Name:  model.MetricNameLabel,
					Value: name,
				})

				for _, k := range labels.OrderedKeys {
					labelPairs = append(labelPairs, prompb.Label{
						Name:  k,
						Value: labels.Map[k],
					})
				}

				ts = &prompb.TimeSeries{
					Labels:  labelPairs,
					Samples: make([]prompb.Sample, 0, 100),
				}
				labelsToSeries[key] = ts
			}

			ts.Samples = append(ts.Samples, prompb.Sample{
				Timestamp: time.UnixNano() / 1000000,
				Value:     value,
			})
		}

		err = rows.Err()
		rows.Close()

		if err != nil {
			return nil, err
		}
	}

	resp := prompb.ReadResponse{
		Results: []*prompb.QueryResult{
			{
				Timeseries: make([]*prompb.TimeSeries, 0, len(labelsToSeries)),
			},
		},
	}
	for _, ts := range labelsToSeries {
		resp.Results[0].Timeseries = append(resp.Results[0].Timeseries, ts)
	}

	level.Debug(c.logger).Log("msg", "Returned response", "#timeseries", len(labelsToSeries))

	return &resp, nil
}

// HealthCheck implements the healtcheck interface
func (c *Client) HealthCheck() error {
	rows, err := c.DB.Query(context.Background(), "SELECT 1")
	defer rows.Close()
	if err != nil {
		level.Debug(c.logger).Log("msg", "Health check error", "err", err)
		return err
	}

	return nil
}

func toTimestamp(milliseconds int64) time.Time {
	sec := milliseconds / 1000
	nsec := (milliseconds - (sec * 1000)) * 1000000
	return time.Unix(sec, nsec).UTC()
}

func (c *Client) buildQuery(q *prompb.Query) (string, error) {
	matchers := make([]string, 0, len(q.Matchers))
	labelEqualPredicates := make(map[string]string)

	for _, m := range q.Matchers {
		escapedName := escapeValue(m.Name)
		escapedValue := escapeValue(m.Value)

		if m.Name == model.MetricNameLabel {
			switch m.Type {
			case prompb.LabelMatcher_EQ:
				if len(escapedValue) == 0 {
					matchers = append(matchers, fmt.Sprintf("(name IS NULL OR name = '')"))
				} else {
					matchers = append(matchers, fmt.Sprintf("name = '%s'", escapedValue))
				}
			case prompb.LabelMatcher_NEQ:
				matchers = append(matchers, fmt.Sprintf("name != '%s'", escapedValue))
			case prompb.LabelMatcher_RE:
				matchers = append(matchers, fmt.Sprintf("name ~ '%s'", anchorValue(escapedValue)))
			case prompb.LabelMatcher_NRE:
				matchers = append(matchers, fmt.Sprintf("name !~ '%s'", anchorValue(escapedValue)))
			default:
				return "", fmt.Errorf("unknown metric name match type %v", m.Type)
			}
		} else {
			switch m.Type {
			case prompb.LabelMatcher_EQ:
				if len(escapedValue) == 0 {
					// From the PromQL docs: "Label matchers that match
					// empty label values also select all time series that
					// do not have the specific label set at all."
					matchers = append(matchers, fmt.Sprintf("((labels ? '%s') = false OR (labels->>'%s' = ''))",
						escapedName, escapedName))
				} else {
					labelEqualPredicates[escapedName] = escapedValue
				}
			case prompb.LabelMatcher_NEQ:
				matchers = append(matchers, fmt.Sprintf("labels->>'%s' != '%s'", escapedName, escapedValue))
			case prompb.LabelMatcher_RE:
				matchers = append(matchers, fmt.Sprintf("labels->>'%s' ~ '%s'", escapedName, anchorValue(escapedValue)))
			case prompb.LabelMatcher_NRE:
				matchers = append(matchers, fmt.Sprintf("labels->>'%s' !~ '%s'", escapedName, anchorValue(escapedValue)))
			default:
				return "", fmt.Errorf("unknown match type %v", m.Type)
			}
		}
	}
	equalsPredicate := ""

	if len(labelEqualPredicates) > 0 {
		labelsJSON, err := json.Marshal(labelEqualPredicates)

		if err != nil {
			return "", err
		}
		equalsPredicate = fmt.Sprintf(" AND labels @> '%s'", labelsJSON)
	}

	matchers = append(matchers, fmt.Sprintf("time >= '%v'", toTimestamp(q.StartTimestampMs).Format(time.RFC3339)))
	matchers = append(matchers, fmt.Sprintf("time <= '%v'", toTimestamp(q.EndTimestampMs).Format(time.RFC3339)))

	return fmt.Sprintf("SELECT time, name, value, labels FROM metrics WHERE %s %s ORDER BY time",
		strings.Join(matchers, " AND "), equalsPredicate), nil
}

func (c *Client) buildCommand(q *prompb.Query) (string, error) {
	return c.buildQuery(q)
}

func escapeValue(str string) string {
	return strings.Replace(str, `'`, `''`, -1)
}

// anchorValue adds anchors to values in regexps since PromQL docs
// states that "Regex-matches are fully anchored."
func anchorValue(str string) string {
	l := len(str)

	if l == 0 || (str[0] == '^' && str[l-1] == '$') {
		return str
	}

	if str[0] == '^' {
		return fmt.Sprintf("%s$", str)
	}

	if str[l-1] == '$' {
		return fmt.Sprintf("^%s", str)
	}

	return fmt.Sprintf("^%s$", str)
}

// Name identifies the client as a PostgreSQL client.
func (c Client) Name() string {
	return "PostgreSQL"
}
