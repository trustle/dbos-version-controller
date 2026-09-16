"""Entry point for ``python -m dbos_version_controller``.

One reconcile every ``DVC_INTERVAL_SECONDS``. SIGTERM finishes the current tick
and exits 0; an unhandled exception in a tick is logged and the loop continues,
because a transient API or database error must not park the controller.
"""

import signal
import threading
from collections.abc import Callable
from datetime import UTC, datetime
from types import FrameType

import psycopg

from dbos_version_controller._log import configure_logging, get_logger
from dbos_version_controller.reconcile import OUTSTANDING_SQL, KubeClient, reconcile
from dbos_version_controller.settings import ControllerSettings

logger = get_logger(__name__)


def _outstanding_counter(database_url: str) -> Callable[[str], int]:
    """Return a callable counting the unfinished workflows of one application version."""

    def outstanding(version: str) -> int:
        with (
            psycopg.connect(database_url, connect_timeout=10, options="-c statement_timeout=10000") as connection,
            connection.cursor() as cursor,
        ):
            cursor.execute(OUTSTANDING_SQL, (version,))
            row = cursor.fetchone()
            return int(row[0]) if row else 0

    return outstanding


def _now() -> datetime:
    return datetime.now(UTC)


def main() -> None:
    settings = ControllerSettings()  # pyright: ignore[reportCallIssue]
    configure_logging(settings.log_level)

    kube = KubeClient(namespace=settings.namespace, dry_run=settings.dry_run)
    outstanding = _outstanding_counter(settings.database_url)

    stopping = threading.Event()

    def _stop(signum: int, frame: FrameType | None) -> None:
        logger.info("Stopping after the current tick", signal=signum)
        stopping.set()

    signal.signal(signal.SIGTERM, _stop)
    signal.signal(signal.SIGINT, _stop)

    logger.info(
        "DBOS version controller started",
        namespace=settings.namespace,
        interval_seconds=settings.interval_seconds,
        grace_seconds=settings.grace_seconds,
        dry_run=settings.dry_run,
    )
    while not stopping.is_set():
        try:
            reconcile(kube=kube, settings=settings, outstanding=outstanding, now=_now)
        except Exception:
            logger.exception("Reconcile tick failed")
        stopping.wait(settings.interval_seconds)
    logger.info("DBOS version controller stopped")


if __name__ == "__main__":
    main()
