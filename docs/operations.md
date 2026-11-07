# Operations: Qdrant failures and index recovery

PostgreSQL is Kasane's source of truth. Qdrant only holds a derived keyword index (collection `kasane_memories_v1`) that Kasane can rebuild from PostgreSQL at any time. **Qdrant data is safe to delete and rebuild.** Back up PostgreSQL instead (for example `pg_dump`). PostgreSQL is the data you cannot regenerate.

## What happens automatically

- **Kasane keeps working without Qdrant.** Reads, writes, listing, MCP tools, and the UI keep working. Search falls back to PostgreSQL full-text search and responses carry `"degraded": true`. Writes queue index jobs that run once Qdrant returns. The Kasane container stays healthy: its healthcheck reports the degraded index but does not fail, so reverse proxies keep routing to it.
- **Degradation is visible.**
  - `GET /api/v1/status` includes `search_index` with `state`, `reason`, `message`, and `since`.
  - `GET /healthz` includes `search_index` and `reason`.
  - `docker inspect --format '{{json .State.Health}}' <app-container>` shows `ok; search index degraded: <reason>`.
  - Kasane logs `WARN search index degraded` when the state changes, then again every 5 minutes for as long as it lasts.
- **The index heals itself.** Every 30 seconds Kasane verifies the collection. If the collection is missing, Kasane creates it. If Qdrant has fewer points than PostgreSQL has indexed memories (for example after a wipe), Kasane logs `ERROR search index is missing points` and queues a full rebuild from PostgreSQL. Search keeps using the PostgreSQL fallback until the rebuild completes (`INFO search index rebuild complete`). Kasane never deletes Qdrant data. A collection with an incompatible configuration is reported as `collection_incompatible` and left for you to fix.
- **Qdrant cannot crash-loop forever.** The `qdrant` service uses `restart: unless-stopped`, so it comes back after a reboot. Its entrypoint is wrapped by [`deploy/qdrant-guard.sh`](../deploy/qdrant-guard.sh), which counts consecutive failed runs: non-zero exits within 5 minutes of starting that `docker stop` did not request. After 5 failures it **parks**. The container stays up but does not start Qdrant, the Qdrant healthcheck reports `unhealthy`, and the logs say `kasane qdrant-guard: ... refusing to start it again`. A clean stop or a run that lasts 5 minutes resets the count. The count is stored in `/qdrant/storage/.kasane-qdrant-failures`, so wiping storage resets it too.

## Reasons reported by Kasane

| Reason | Meaning |
| --- | --- |
| `not_checked` | Startup; the first check runs within seconds |
| `qdrant_unreachable` | Qdrant is down, parked, or unreachable on the network |
| `qdrant_error` | Qdrant answered with an error |
| `collection_incompatible` | The collection exists with an unexpected configuration; recover manually |
| `database_unavailable` | Kasane could not compare the index with PostgreSQL |
| `index_rebuilding` | A rebuild from PostgreSQL is in progress |

## Symptoms of a corrupted Qdrant store

- Kasane reports `qdrant_unreachable` for more than a few minutes.
- The Qdrant container is `unhealthy`, or its logs contain `Bus error` or a `qdrant-guard` message.
- Exit code 135 (SIGBUS) or 139 (SIGSEGV), and a growing restart count:

  ```sh
  docker ps -a --filter name=qdrant --format '{{.Names}} {{.Status}}'
  docker inspect -f 'restarts={{.RestartCount}} exit={{.State.ExitCode}}' <qdrant-container>
  docker logs --tail 50 <qdrant-container>
  ```

SIGBUS while Qdrant opens its files almost always means the storage medium returned an I/O error on a memory-mapped read. That is usually an unreadable block left by power loss on a drive without power-loss protection. Check the kernel log and the drive:

```sh
journalctl -k | grep -iE "medium error|i/o error|blk_update_request|nvme"
sudo smartctl -a /dev/nvme0        # Media and Data Integrity Errors, Unsafe Shutdowns
sudo nvme smart-log /dev/nvme0     # same counters via nvme-cli
```

A rising media-error count means the drive is degrading. Check PostgreSQL backups first and plan to replace the drive or add a UPS. This runbook only rebuilds the derived index.

## Recovery: rebuild Qdrant from PostgreSQL

Commands assume the Compose service names `qdrant` and `app`. On Dokploy, find the container names with `docker ps --filter name=<compose-app-name>`, or run `docker compose -p <compose-app-name> ...` from the deployment directory.

1. **Stop Qdrant** so it stops touching the bad blocks:

   ```sh
   docker stop <qdrant-container>
   ```

2. **Set the old storage aside.** Do not copy it. Reading bad blocks fails and stresses the drive further. Renaming is instant and keeps the data for later inspection. With `QDRANT_STORAGE` set to a host path, for example `/mnt/data2tb/kasane/qdrant`:

   ```sh
   sudo mv /mnt/data2tb/kasane/qdrant /mnt/data2tb/kasane/qdrant.broken-$(date +%F)
   sudo mkdir /mnt/data2tb/kasane/qdrant
   ```

   With the default named volume, remove the volume instead: `docker rm <qdrant-container>`, then `docker volume rm <project>_qdrant-data`.

3. **Start Qdrant again** with `docker start <qdrant-container>`. If you removed the container in the named-volume case, redeploy the Compose app or run `docker compose up -d qdrant` instead. Qdrant starts empty and the guard counter starts at zero.

4. **Let Kasane rebuild.** Within about 30 seconds, Kasane creates `kasane_memories_v1` and queues every live memory:

   ```sh
   docker logs -f <app-container> 2>&1 | grep -i "search index"
   ```

   `GET /api/v1/status` returns `search_index.state: "ready"` once the rebuild finishes. To force a rebuild manually (it is idempotent), run `docker exec <app-container> kasane reindex`. The running server processes the queue.

5. Once search works, delete the `qdrant.broken-*` directory.

## Unparking Qdrant without wiping

If the failures had a transient cause, such as a full disk or a bad configuration, fix it, clear the counter, and restart:

```sh
docker exec <qdrant-container> rm /qdrant/storage/.kasane-qdrant-failures
docker restart <qdrant-container>
```

If Qdrant fails again five times, use the recovery procedure above.
