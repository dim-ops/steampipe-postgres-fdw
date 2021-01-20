package hub

import (
	"fmt"
	"log"
	"sync"

	"github.com/turbot/steampipe-plugin-sdk/grpc/proto"
	"github.com/turbot/steampipe-plugin-sdk/logging"
	"github.com/turbot/steampipe-postgres-fdw/types"
	"github.com/turbot/steampipe/connection_config"
)

const (
	rowBufferSize    = 100
	defaultPluginDir = `~/.steampipe/providers`
)

// Hub :: structure representing plugin hub
type Hub struct {
	connections      *connectionMap
	connectionConfig *connection_config.ConnectionConfig
}

// global hub instance
var hubSingleton *Hub

// mutex protecting creation
var hubMux sync.Mutex

//// lifecycle ////

// GetHub :: return hub singleton
// if there is an existing hub singleton instance return it, otherwise create it
// if a hub exists, but a different pluginDir is specified, reinitialise the hub with the new dir
func GetHub() (*Hub, error) {
	logging.LogTime("GetHub start")

	// lock access to singleton
	hubMux.Lock()
	defer hubMux.Unlock()

	var err error
	if hubSingleton == nil {
		hubSingleton, err = newHub()
		if err != nil {
			return nil, err
		}
	}
	logging.LogTime("GetHub end")
	return hubSingleton, err
}

func newHub() (*Hub, error) {
	log.Println("[DEBUG] newHub")
	connectionConfig, err := connection_config.Load()
	if err != nil {
		return nil, err
	}

	connections := newConnectionMap()

	hub := &Hub{connections, connectionConfig}

	for connectionName, connectionConfig := range hub.connectionConfig.Connections {
		log.Printf("[DEBUG] create connection %s, plugin %s", connectionName, connectionConfig.Plugin)

		if _, err := hub.createConnectionPlugin(connection_config.PluginFQNToSchemaName(connectionConfig.Plugin), connectionName); err != nil {
			return nil, err
		}
	}

	return hub, nil
}

// shutdown all plugin clients
func (h *Hub) Close() {
	log.Println("[TRACE] close")
	for _, connection := range h.connections.connectionPlugins {
		connection.Plugin.Client.Kill()
	}
}

//// public fdw functions ////

// GetSchema :: return the schema for a name. Load the plugin for the connection if needed
func (h *Hub) GetSchema(remoteSchema string, localSchema string) (*proto.Schema, error) {
	pluginFQN := remoteSchema
	connectionName := localSchema
	log.Printf("[DEBUG] GetSchema remoteSchema: %s, name %s\n", remoteSchema, connectionName)

	c := h.connections.get(pluginFQN, connectionName)

	// if we do not have this ConnectionPlugin loaded, create
	if c == nil {
		log.Printf("[TRACE] connection plugin is not loaded - loading\n")

		var err error
		c, err = h.createConnectionPlugin(pluginFQN, connectionName)
		if err != nil {
			return nil, err
		}
	}

	return c.Schema, nil
}

// Scan :: Start a table scan. Returns an iterator
func (h *Hub) Scan(rel *types.Relation, columns []string, quals []*proto.Qual, opts types.Options) (Iterator, error) {
	logging.LogTime("Scan start")
	iterator := newIterator(h, rel, opts)
	err := h.startScan(iterator, columns, quals)
	logging.LogTime("Scan end")
	return iterator, err
}

// GetRelSize ::  Method called from the planner to estimate the resulting relation size for a scan.
//        It will help the planner in deciding between different types of plans,
//        according to their costs.
//        Args:
//            columns (list): The list of columns that must be returned.
//            quals (list): A list of Qual instances describing the filters
//                applied to this scan.
//        Returns:
//            A struct of the form (expected_number_of_rows, avg_row_width (in bytes))
func (h *Hub) GetRelSize(columns []string, quals []*proto.Qual, opts types.Options) (types.RelSize, error) {
	log.Println("[TRACE] GetRelSize")
	result := types.RelSize{
		// Default to 1M rows, because these tables are typically expensive
		// relative to standard postgres.
		Rows: 1000000,
		// Width is in bytes, assuming an average of 100 per column.
		Width: 100 * len(columns),
	}
	return result, nil
}

// GetPathKeys ::  Method called from the planner to add additional Path to the planner.
//        By default, the planner generates an (unparameterized) path, which
//        can be reasoned about like a SequentialScan, optionally filtered.
//        This method allows the implementor to declare other Paths,
//        corresponding to faster access methods for specific attributes.
//        Such a parameterized path can be reasoned about like an IndexScan.
//        For example, with the following query::
//            select * from foreign_table inner join local_table using(id);
//        where foreign_table is a foreign table containing 100000 rows, and
//        local_table is a regular table containing 100 rows.
//        The previous query would probably be transformed to a plan similar to
//        this one::
//            ┌────────────────────────────────────────────────────────────────────────────────────┐
//            │                                     QUERY PLAN                                     │
//            ├────────────────────────────────────────────────────────────────────────────────────┤
//            │ Hash Join  (cost=57.67..4021812.67 rows=615000 width=68)                           │
//            │   Hash Cond: (foreign_table.id = local_table.id)                                   │
//            │   ->  Foreign Scan on foreign_table (cost=20.00..4000000.00 rows=100000 width=40)  │
//            │   ->  Hash  (cost=22.30..22.30 rows=1230 width=36)                                 │
//            │         ->  Seq Scan on local_table (cost=0.00..22.30 rows=1230 width=36)          │
//            └────────────────────────────────────────────────────────────────────────────────────┘
//        But with a parameterized path declared on the id key, with the knowledge that this key
//        is unique on the foreign side, the following plan might get chosen::
//            ┌───────────────────────────────────────────────────────────────────────┐
//            │                              QUERY PLAN                               │
//            ├───────────────────────────────────────────────────────────────────────┤
//            │ Nested Loop  (cost=20.00..49234.60 rows=615000 width=68)              │
//            │   ->  Seq Scan on local_table (cost=0.00..22.30 rows=1230 width=36)   │
//            │   ->  Foreign Scan on remote_table (cost=20.00..40.00 rows=1 width=40)│
//            │         Filter: (id = local_table.id)                                 │
//            └───────────────────────────────────────────────────────────────────────┘
//        Returns:
//            A list of tuples of the form: (key_columns, expected_rows),
//            where key_columns is a tuple containing the columns on which
//            the path can be used, and expected_rows is the number of rows
//            this path might return for a simple lookup.
//            For example, the return value corresponding to the previous scenario would be::
//                [(('id',), 1)]
func (h *Hub) GetPathKeys(opts types.Options) ([]types.PathKey, error) {
	log.Println("[TRACE] GetPathKeys")
	return make([]types.PathKey, 0), nil
}

// Explain ::  hook called on explain.
//        Returns:
//            An iterable of strings to display in the EXPLAIN output.
func (h *Hub) Explain(columns []string, quals []*proto.Qual, sortKeys []string, verbose bool, opts types.Options) ([]string, error) {
	log.Println("[TRACE] Explain")
	return make([]string, 0), nil
}

//// internal implementation ////

// split startScan into a separate function to allow iterator to restart the scan
func (h *Hub) startScan(iterator *scanIterator, columns []string, quals []*proto.Qual) error {
	table := iterator.opts["table"]
	// get the namespace (i.e. the local schema) - this is the connection name
	connection := iterator.rel.Namespace

	log.Printf("[INFO] StartScan\n  table: %s\n  columns: %v\n", table, columns)
	// get ConnectionPlugin which serves this table
	c, err := h.connections.getConnectionPluginForTable(table, connection)
	if err != nil {
		return err
	}

	qualMap, err := h.buildQualMap(quals)

	if err != nil {
		return err
	}
	var queryContext = &proto.QueryContext{
		Columns: columns,
		Quals:   qualMap,
	}

	// if a scanIterator is in progress, fail
	if iterator.inProgress() {
		return fmt.Errorf("cannot start scanIterator while existing scanIterator is incomplete - cancel first")
	}

	req := &proto.ExecuteRequest{
		Table:        table,
		QueryContext: queryContext,
	}
	log.Println("[TRACE] stub execute")
	str, err := c.Plugin.Stub.Execute(req)
	if err != nil {
		log.Printf("[TRACE] set iterator error %v\n", err)
		iterator.setError(err)
		return err
	}
	log.Println("[TRACE] stub execute returned")

	log.Println("[TRACE] got stream")
	iterator.start(str)
	log.Println("[TRACE] iterator start returned")
	return nil
}

// load the given plugin connection into the connection map and return the schema
func (h *Hub) createConnectionPlugin(pluginFQN, connectionName string) (*connection_config.ConnectionPlugin, error) {
	opts := &connection_config.ConnectionPluginOptions{PluginFQN: pluginFQN, ConnectionName: connectionName}
	c, err := connection_config.CreateConnectionPlugin(opts)
	if err != nil {
		return nil, err
	}
	if err = h.connections.add(c); err != nil {
		return nil, err
	}
	return c, nil
}