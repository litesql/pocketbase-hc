# PocketBase HC

PocketBase HC runs [PocketBase](https://pocketbase.io/) as a highly consistent cluster. It uses the [`go-ha`](https://github.com/litesql/go-ha) `database/sql` driver to replicate database changes between nodes over gRPC.

## Features

- Transactionally consistent replication between PocketBase nodes.
- Remote SQL access through an optional, authenticated gRPC endpoint.
- Transaction history and the ability to undo committed transactions.
- Compatibility with terminal clients and [DBeaver](https://github.com/litesql/jdbc-ha#dbeaver-integration).

## Requirements

- Go `1.27` or later when building from source.

## Installation

Download a binary from the [latest release](https://github.com/litesql/pocketbase-hc/releases/latest/), install from source, or use the Docker image:

```sh
go install github.com/litesql/pocketbase-hc@latest
```

### Docker image

```sh
docker pull ghcr.io/litesql/pocketbase-hc:latest
```

## Configuration

Configure each node with environment variables:

| Variable | Description | Default |
| --- | --- | --- |
| `PB_NAME` | Unique node name. | System hostname (`$HOSTNAME`) |
| `PB_PEERS` | Comma-separated gRPC URLs of the other nodes to replicate to. | Empty |
| `PB_2PC_TIMEOUT` | Two-phase commit timeout. | `10s` |
| `PB_GRPC_PORT` | Port for the gRPC service and remote database access. | Disabled |
| `PB_GRPC_TOKEN` | Token required for remote gRPC access. | No authentication |
| `PB_SUPERUSER_EMAIL` | Email for a superuser created at startup. | Empty |
| `PB_SUPERUSER_PASS` | Password for a superuser created at startup. | Empty |

Use the gRPC port of each node in `PB_PEERS`. For example, `PB_PEERS=http://localhost:9091`.

## Usage

### Starting a Cluster

1. Start the first PocketBase HA instance:

    ```sh
    PB_NAME=peer1 PB_PEERS=http://localhost:9091 PB_GRPC_PORT=9090 pocketbase-hc serve
    
    ```    

2. Start a second instance in a different directory:

    ```sh
    PB_NAME=peer2 PB_PEERS=http://localhost:9090 PB_GRPC_PORT=9091 pocketbase-hc serve --http 127.0.0.1:8091
    ```

> **Note**: You can skip setting the superuser password for the peer1 instance.

### Running a Cluster with docker

To run a pocketbase-hc cluster using Docker Compose, use the following command:

```sh
docker compose up
```

- Superuser e-mail: test@example.com
- Superuser pass: 1234567890

You can define the superuser password using this command:

```sh
docker compose exec node1 /app/pocketbase-hc superuser upsert EMAIL PASS
```

Access the three nodes using the following address:

- Node1: http://localhost:8090
- Node2: http://localhost:8091
- Node3: http://localhost:8092

> **Tip**: Ensure all nodes are synchronized by verifying the logs or using the PocketBase admin interface.

### Event Hooks on replica nodes

On replica nodes (the nodes that users do not directly interact with), only the following events are triggered by the PocketBase event hooks system:

- OnModelAfterCreateSuccess
- OnModelAfterCreateError
- OnModelAfterUpdateSuccess
- OnModelAfterUpdateError
- OnModelAfterDeleteSuccess
- OnModelAfterDeleteError

### Data Replication

**PocketBase HC** uses a two-phase commit strategy for handling transactions.

For applications that require higher availability, consider using [PocketBase HA](https://github.com/litesql/pocketbase-ha).

## Remote database access

Enable the gRPC endpoint and connect from another terminal:

```sh
PB_GRPC_PORT=9090 pocketbase-hc serve
pocketbase-hc remote http://localhost:9090
```

The client accepts SQL commands, for example:

```text
data.db> SELECT * FROM PRAGMA_table_list;
```

### Authentication

Set `PB_GRPC_TOKEN` on the server and pass the same token to the client:

```sh
PB_GRPC_PORT=9090 PB_GRPC_TOKEN=secret pocketbase-hc serve
pocketbase-hc remote http://localhost:9090 --token secret
```

#### Undo transactions

You can undo transactions using `pocketbase-hc cli` UNDO command:

```sh
# connect
pocketbase-hc remote http://host:port

# undo latest transaction
undo;

# undo last N transactions
undo N;

# undo transactions since time. Ex: undo 5m; (5 minutes ago)
undo <time duration>;

# show the latest transaction
history;

# show the last N transactions
history N;

# show the transactions since time.
history <time duration>;
```

## Contributing

Contributions are welcome! Feel free to open issues or submit pull requests to improve this project.


## License

This project is licensed under the [MIT License](LICENSE).

