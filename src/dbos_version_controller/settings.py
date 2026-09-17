"""Settings for the DBOS version controller."""

from pydantic import Field
from pydantic_settings import BaseSettings, SettingsConfigDict


class ControllerSettings(BaseSettings):
    """Settings for ``python -m dbos_version_controller``.

    Git holds one ``WorkerVersionSet`` per DBOS application; this controller
    owns every per-version worker object the set covers. Every field reads from
    an environment variable with the ``DVC_`` prefix.
    """

    namespace: str = Field(
        description="Namespace holding the WorkerVersionSets and the worker objects.",
    )
    interval_seconds: float = Field(
        default=30.0,
        gt=0,
        description="Seconds between reconcile ticks.",
    )
    grace_seconds: float = Field(
        default=600.0,
        ge=0,
        description="Default seconds a drained version stays at zero rows and zero pods before deletion.",
    )
    dry_run: bool = Field(
        default=False,
        description="Log every create/patch/delete as `would ...` and perform no write.",
    )
    managed_by: str = Field(
        default="dbos-version-controller",
        description="Value of the `app.kubernetes.io/managed-by` label identifying objects this controller owns.",
    )
    version_label: str = Field(
        default="dbos-app-version",
        description="Label carrying the DBOS application version on each managed object.",
    )
    database_url: str = Field(
        description="URL of the DBOS system database, read to count outstanding workflows.",
    )
    log_level: str = Field(
        default="INFO",
        description="Level of the root logger.",
    )

    model_config = SettingsConfigDict(env_prefix="dvc_")
