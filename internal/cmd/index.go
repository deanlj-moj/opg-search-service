package cmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/ministryofjustice/opg-search-service/internal/firm"
	"github.com/ministryofjustice/opg-search-service/internal/index"
	"github.com/ministryofjustice/opg-search-service/internal/person"
	"github.com/sirupsen/logrus"
)

type Secrets interface {
	GetGlobalSecretString(key string) (string, error)
}

type IndexCommand struct {
	logger            *logrus.Logger
	esClient          index.BulkClient
	secrets           Secrets
	currentIndexNames []string
}

type IndexConfig struct {
	// simple alias for the index (e.g. digital_lpa, firm, person)
	Alias string

	// real name of the index on open search server inc. 8 digit hash
	Name string

	// configuration for the index
	Config []byte
}

func NewIndexConfig(configFunc func() ([]byte, error), alias string, l *logrus.Logger) IndexConfig {
	config, err := configFunc()
	if err != nil {
		l.Fatal(err)
	}

	sum := sha256.Sum256(config)
	indexName := fmt.Sprintf("%s_%x", alias, sum[:8])

	return IndexConfig{
		Name:   indexName,
		Alias:  alias,
		Config: config,
	}
}

func NewIndex(logger *logrus.Logger, esClient index.BulkClient, secrets Secrets, indexes []IndexConfig) *IndexCommand {
	var indexNames []string
	for _, indexConfig := range indexes {
		indexNames = append(indexNames, indexConfig.Name)
	}
	return &IndexCommand{
		logger:            logger,
		esClient:          esClient,
		secrets:           secrets,
		currentIndexNames: indexNames,
	}
}

func (c *IndexCommand) Info() (name, description string) {
	return "index", "index records"
}

func (c *IndexCommand) Run(args []string) error {
	flagset := flag.NewFlagSet("index", flag.ExitOnError)

	all := flagset.Bool("all", false, "index all records for chosen indices")
	firmOnly := flagset.Bool("firm", false, "index records to the firm index")
	personOnly := flagset.Bool("person", false, "index records to the person index")
	from := flagset.Int("from", 0, "index an id range starting from (use with -to)")
	to := flagset.Int("to", 100, "index an id range ending at (use with -from)")
	batchSize := flagset.Int("batch-size", 10000, "batch size to read from db")
	fromDate := flagset.String("from-date", "", "index records updated from this date")

	if err := flagset.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()

	connString, err := c.dbConnectionString()
	if err != nil {
		return err
	}

	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return err
	}
	defer conn.Close(ctx) //nolint:errcheck // no need to check error when closing DB connection

	if err := conn.Ping(ctx); err != nil {
		return err
	}

	indexers := map[string]*index.Indexer{}
	noneSet := !*firmOnly && !*personOnly

	if *firmOnly || noneSet {
		for _, indexName := range c.currentIndexNames {
			if strings.HasPrefix(indexName, "firm_") {
				indexers["firm"] = index.New(c.esClient, c.logger, firm.NewDB(conn), indexName)
				break
			}
		}
	}
	if *personOnly || noneSet {
		for _, indexName := range c.currentIndexNames {
			if strings.HasPrefix(indexName, "person_") {
				indexers["person"] = index.New(c.esClient, c.logger, person.NewDB(conn), indexName)
				break
			}
		}
	}

	fromTime, err := time.Parse(time.RFC3339, *fromDate)

	if *fromDate != "" && err != nil {
		return fmt.Errorf("-from-date: %w", err)
	}

	for indexerName, indexer := range indexers {
		var result *index.Result

		if !fromTime.IsZero() {
			c.logger.Printf("indexing %s by date from=%v batchSize=%d", indexerName, fromTime, *batchSize)
			result, err = indexer.FromDate(ctx, fromTime, *batchSize)
		} else if *all {
			c.logger.Printf("indexing %s all records batchSize=%d", indexerName, *batchSize)
			result, err = indexer.All(ctx, *batchSize)
		} else {
			c.logger.Printf("indexing %s by id from=%d to=%d batchSize=%d", indexerName, *from, *to, *batchSize)
			result, err = indexer.ByID(ctx, *from, *to, *batchSize)
		}

		if err != nil {
			return err
		}

		c.logger.Printf("indexing done successful=%d failed=%d", result.Successful, result.Failed)
		for _, e := range result.Errors {
			c.logger.Println(e)
		}
	}

	return nil
}

func (c *IndexCommand) dbConnectionString() (string, error) {
	pass := os.Getenv("SEARCH_SERVICE_DB_PASS")
	if passSecret := os.Getenv("SEARCH_SERVICE_DB_PASS_SECRET"); passSecret != "" {
		var err error
		pass, err = c.secrets.GetGlobalSecretString(passSecret)
		if err != nil {
			return "", err
		}
	}
	if pass == "" {
		return "", errors.New("SEARCH_SERVICE_DB_PASS or SEARCH_SERVICE_DB_PASS_SECRET must be specified")
	}

	user, host, port, database := os.Getenv("SEARCH_SERVICE_DB_USER"), os.Getenv("SEARCH_SERVICE_DB_HOST"), os.Getenv("SEARCH_SERVICE_DB_PORT"), os.Getenv("SEARCH_SERVICE_DB_DATABASE")
	if user == "" {
		return "", errors.New("SEARCH_SERVICE_DB_USER must be specified")
	}
	if host == "" {
		return "", errors.New("SEARCH_SERVICE_DB_HOST must be specified")
	}
	if port == "" {
		return "", errors.New("SEARCH_SERVICE_DB_PORT must be specified")
	}
	if database == "" {
		return "", errors.New("SEARCH_SERVICE_DB_DATABASE must be specified")
	}

	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s", user, url.QueryEscape(pass), host, port, database), nil
}
