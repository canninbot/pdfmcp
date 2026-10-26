#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.14"
# dependencies = [
#   "python-dotenv>=1.2.1",
#   "requests>=2.32.5",
# ]
# ///

"""Annotate PMIDs with PMC, OpenAlex, and SCImago metadata in one file."""

import argparse
import csv
import email.utils
import json
import math
import os
import random
import re
import sqlite3
import sys
import tempfile
import time
from collections.abc import Sequence
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import requests
from dotenv import load_dotenv

# ENVIRONMENT CONFIGURATION

SCRIPT_DIRECTORY = Path(__file__).resolve().parent
SCRIPT_DOTENV_PATH = SCRIPT_DIRECTORY / ".env"


def dotenv_search_paths() -> tuple[Path, ...]:
    """Return supported dotenv paths in increasing precedence order.

    The current working directory is checked so running the script from a
    project root uses that project's ``.env``. A file beside the script remains
    supported for standalone use. If both exist, the working-directory file
    takes precedence.

    Returns:
        Unique dotenv paths, with the highest-precedence path last.
    """
    working_directory_path = Path.cwd().resolve() / ".env"
    if working_directory_path == SCRIPT_DOTENV_PATH:
        return (SCRIPT_DOTENV_PATH,)
    return SCRIPT_DOTENV_PATH, working_directory_path


def load_local_environment() -> tuple[Path, ...]:
    """Load configuration from each supported dotenv location.

    Returns:
        Existing dotenv paths that were loaded, in precedence order.
    """
    loaded_paths: list[Path] = []
    for dotenv_path in dotenv_search_paths():
        if dotenv_path.is_file():
            load_dotenv(dotenv_path=dotenv_path, override=True)
            loaded_paths.append(dotenv_path)
    return tuple(loaded_paths)


# PMC ID CONVERTER IMPLEMENTATION

pmc_PMC_ID_CONVERTER_URL = "https://pmc.ncbi.nlm.nih.gov/tools/idconv/api/v1/articles/"
pmc_DEFAULT_DATABASE_PATH = Path("openalex_cache.sqlite")
pmc_DEFAULT_BATCH_SIZE = 200
pmc_MAX_BATCH_SIZE = 200
pmc_DEFAULT_MAX_RETRIES = 6
pmc_DEFAULT_DELAY_SECONDS = 0.4
pmc_DEFAULT_CONNECT_TIMEOUT_SECONDS = 10.0
pmc_DEFAULT_READ_TIMEOUT_SECONDS = 60.0
pmc_DEFAULT_TOOL_NAME = "openalex_citations_pipeline"
pmc_TRANSIENT_HTTP_STATUSES = {408, 429, 500, 502, 503, 504}
pmc_FATAL_HTTP_STATUSES = {400, 401, 403, 404, 422}
pmc_PMID_PATTERN = re.compile(r"^[0-9]+$")


class pmc_InputError(Exception):
    """Raised when an input PMID file is invalid."""


class pmc_FatalApiError(Exception):
    """Raised when further PMC requests require intervention."""


class pmc_BatchRequestError(Exception):
    """Raised when one batch cannot be retrieved after bounded retries."""


class pmc_ResponseFormatError(Exception):
    """Raised when a PMC response cannot be interpreted safely."""


@dataclass(frozen=True)
class pmc_PmcRecord:
    """Represent one completed PMC lookup result."""

    pmid: str
    pmcid: str | None
    pmc_fulltext_available: int
    release_date: str | None
    doi: str | None
    checked_at: str


@dataclass
class pmc_RequestStatistics:
    """Track HTTP attempts and successful logical lookup calls."""

    http_requests: int = 0
    successful_lookup_requests: int = 0


def pmc_nonnegative_integer(value: str) -> int:
    """Parse a nonnegative integer CLI value.

    Args:
        value: Raw argument value.

    Returns:
        Parsed nonnegative integer.

    Raises:
        argparse.ArgumentTypeError: If validation fails.
    """
    try:
        parsed_value = int(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError("must be an integer") from error
    if parsed_value < 0:
        raise argparse.ArgumentTypeError("must be zero or greater")
    return parsed_value


def pmc_positive_batch_size(value: str) -> int:
    """Parse a PMC batch size from 1 through 200.

    Args:
        value: Raw argument value.

    Returns:
        Validated batch size.

    Raises:
        argparse.ArgumentTypeError: If validation fails.
    """
    parsed_value = pmc_nonnegative_integer(value)
    if not 1 <= parsed_value <= pmc_MAX_BATCH_SIZE:
        raise argparse.ArgumentTypeError(f"must be between 1 and {pmc_MAX_BATCH_SIZE}")
    return parsed_value


def pmc_nonnegative_float(value: str) -> float:
    """Parse a nonnegative floating-point CLI value.

    Args:
        value: Raw argument value.

    Returns:
        Parsed nonnegative float.

    Raises:
        argparse.ArgumentTypeError: If validation fails.
    """
    try:
        parsed_value = float(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError("must be a number") from error
    if parsed_value < 0:
        raise argparse.ArgumentTypeError("must be zero or greater")
    return parsed_value


def pmc_parse_arguments(argv: Sequence[str] | None = None) -> argparse.Namespace:
    """Parse command-line options.

    Args:
        argv: Optional argument sequence.

    Returns:
        Parsed arguments.
    """
    parser = argparse.ArgumentParser(
        description=(
            "Retrieve PMC IDs and live full-text status for PMID files using a "
            "durable SQLite cache."
        )
    )
    parser.add_argument(
        "pmid_files",
        nargs="+",
        type=Path,
        help="One or more text files containing one PMID per line",
    )
    parser.add_argument(
        "--db",
        type=Path,
        default=pmc_DEFAULT_DATABASE_PATH,
        help=f"SQLite cache path (default: {pmc_DEFAULT_DATABASE_PATH})",
    )
    parser.add_argument(
        "--batch-size",
        type=pmc_positive_batch_size,
        default=pmc_DEFAULT_BATCH_SIZE,
        help="PMIDs per PMC request, 1-200 (default: 200)",
    )
    parser.add_argument(
        "--max-retries",
        type=pmc_nonnegative_integer,
        default=pmc_DEFAULT_MAX_RETRIES,
        help="Retries after the initial request (default: 6)",
    )
    parser.add_argument(
        "--delay",
        type=pmc_nonnegative_float,
        default=pmc_DEFAULT_DELAY_SECONDS,
        metavar="SECONDS",
        help="Delay after successful requests (default: 0.4)",
    )
    parser.add_argument(
        "--refresh-all",
        action="store_true",
        help="Deliberately re-query all input PMIDs",
    )
    parser.add_argument(
        "--email",
        help="NCBI contact email; defaults to NCBI_EMAIL from .env",
    )
    parser.add_argument(
        "--tool",
        default=pmc_DEFAULT_TOOL_NAME,
        help=f"NCBI tool identifier (default: {pmc_DEFAULT_TOOL_NAME})",
    )
    return parser.parse_args(argv)


def pmc_read_pmids(paths: Sequence[Path]) -> tuple[int, list[str]]:
    """Read and order-deduplicate numeric PMIDs across input files.

    Args:
        paths: PMID text-file paths in precedence order.

    Returns:
        Total nonblank rows and ordered unique PMIDs.

    Raises:
        pmc_InputError: If a file is unreadable, empty, or contains invalid PMIDs.
    """
    total_pmids = 0
    unique_pmids: list[str] = []
    seen_pmids: set[str] = set()
    invalid_examples: list[str] = []
    invalid_count = 0

    for path in paths:
        try:
            with path.open("r", encoding="utf-8-sig") as input_file:
                for line_number, raw_line in enumerate(input_file, start=1):
                    pmid = raw_line.strip()
                    if not pmid:
                        continue
                    total_pmids += 1
                    if not pmc_PMID_PATTERN.fullmatch(pmid):
                        invalid_count += 1
                        if len(invalid_examples) < 10:
                            invalid_examples.append(
                                f"{path}: line {line_number}: {pmid!r}"
                            )
                        continue
                    if pmid not in seen_pmids:
                        seen_pmids.add(pmid)
                        unique_pmids.append(pmid)
        except (OSError, UnicodeError) as error:
            raise pmc_InputError(f"Cannot read PMID file '{path}': {error}") from error

    if invalid_examples:
        suffix = " (showing first 10)" if invalid_count > 10 else ""
        raise pmc_InputError(
            f"Non-numeric PMID values found: {', '.join(invalid_examples)}{suffix}"
        )
    if not unique_pmids:
        raise pmc_InputError("No PMIDs found in the supplied files")
    return total_pmids, unique_pmids


def pmc_connect_database(path: Path) -> sqlite3.Connection:
    """Open SQLite and initialize the durable PMC cache table.

    Args:
        path: SQLite database path.

    Returns:
        Initialized SQLite connection.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(path, timeout=30.0)
    connection.execute("PRAGMA busy_timeout = 30000")
    connection.execute("PRAGMA journal_mode = WAL")
    connection.execute("PRAGMA synchronous = FULL")
    connection.execute(
        """
        CREATE TABLE IF NOT EXISTS pmc_cache (
            pmid TEXT PRIMARY KEY,
            pmcid TEXT,
            pmc_fulltext_available INTEGER NOT NULL
                CHECK (pmc_fulltext_available IN (0, 1)),
            release_date TEXT,
            doi TEXT,
            checked_at TEXT NOT NULL
        )
        """
    )
    connection.commit()
    return connection


def pmc_load_input_table(
    connection: sqlite3.Connection,
    pmids: Sequence[str],
) -> None:
    """Load ordered unique PMIDs into a temporary SQLite table.

    Args:
        connection: Open SQLite connection.
        pmids: Ordered unique PMIDs.
    """
    with connection:
        connection.execute("DROP TABLE IF EXISTS temp.input_pmids")
        connection.execute(
            """
            CREATE TEMPORARY TABLE input_pmids (
                position INTEGER PRIMARY KEY,
                pmid TEXT NOT NULL UNIQUE
            )
            """
        )
        connection.executemany(
            "INSERT INTO input_pmids (position, pmid) VALUES (?, ?)",
            enumerate(pmids),
        )


def pmc_select_pmids_to_query(
    connection: sqlite3.Connection,
    refresh_all: bool,
) -> list[str]:
    """Select unchecked PMIDs in original union order.

    Args:
        connection: Open SQLite connection with the input table loaded.
        refresh_all: Whether all PMIDs should be rechecked.

    Returns:
        PMIDs requiring a PMC request.
    """
    if refresh_all:
        rows = connection.execute("SELECT pmid FROM input_pmids ORDER BY position")
    else:
        rows = connection.execute(
            """
            SELECT input_pmids.pmid
            FROM input_pmids
            LEFT JOIN pmc_cache ON pmc_cache.pmid = input_pmids.pmid
            WHERE pmc_cache.pmid IS NULL
            ORDER BY input_pmids.position
            """
        )
    return [row[0] for row in rows]


def pmc_utc_timestamp() -> str:
    """Return the current UTC timestamp in stable ISO format.

    Returns:
        UTC timestamp ending in ``Z``.
    """
    return datetime.now(UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def pmc_optional_text(value: Any) -> str | None:
    """Return stripped nonempty text or ``None``.

    Args:
        value: Potential text value.

    Returns:
        Stripped text when available.
    """
    if not isinstance(value, str):
        return None
    stripped_value = value.strip()
    return stripped_value or None


def pmc_parse_pmc_results(
    payload: Any, requested_pmids: Sequence[str]
) -> list[pmc_PmcRecord]:
    """Map a successful PMC response to every requested PMID.

    Args:
        payload: Parsed PMC JSON response.
        requested_pmids: PMIDs included in the request.

    Returns:
        One completed cache record per requested PMID.

    Raises:
        pmc_ResponseFormatError: If the response structure is unsafe to interpret.
    """
    if not isinstance(payload, dict) or not isinstance(payload.get("records"), list):
        raise pmc_ResponseFormatError("response does not contain a records list")

    requested_set = set(requested_pmids)
    records_by_pmid: dict[str, dict[str, Any]] = {}
    for record in payload["records"]:
        if not isinstance(record, dict):
            raise pmc_ResponseFormatError("response contains a non-object record")
        requested_id_value = record.get("requested-id")
        if requested_id_value is None:
            requested_id_value = record.get("pmid")
        requested_id = str(requested_id_value) if requested_id_value is not None else ""
        if requested_id not in requested_set:
            raise pmc_ResponseFormatError("response contains an unrequested PMID")
        if requested_id in records_by_pmid:
            raise pmc_ResponseFormatError("response contains duplicate PMID records")
        records_by_pmid[requested_id] = record

    checked_at = pmc_utc_timestamp()
    parsed_records: list[pmc_PmcRecord] = []
    for pmid in requested_pmids:
        record = records_by_pmid.get(pmid, {})
        pmcid = pmc_optional_text(record.get("pmcid"))
        live_value = record.get("live")
        is_live = pmcid is not None and live_value not in (
            False,
            "false",
            "False",
            0,
            "0",
        )
        parsed_records.append(
            pmc_PmcRecord(
                pmid=pmid,
                pmcid=pmcid,
                pmc_fulltext_available=int(is_live),
                release_date=pmc_optional_text(record.get("release-date")),
                doi=pmc_optional_text(record.get("doi")),
                checked_at=checked_at,
            )
        )
    return parsed_records


def pmc_retry_after_seconds(response: requests.Response) -> float | None:
    """Parse Retry-After as seconds or an HTTP date.

    Args:
        response: HTTP response with optional Retry-After.

    Returns:
        Nonnegative delay or ``None``.
    """
    header_value = response.headers.get("Retry-After")
    if not header_value:
        return None
    try:
        return max(0.0, float(header_value))
    except ValueError:
        try:
            retry_time = email.utils.parsedate_to_datetime(header_value)
        except (TypeError, ValueError, OverflowError):
            return None
        if retry_time.tzinfo is None:
            retry_time = retry_time.replace(tzinfo=UTC)
        return max(0.0, (retry_time - datetime.now(UTC)).total_seconds())


def pmc_retry_delay(retry_number: int, server_delay: float | None = None) -> float:
    """Calculate exponential backoff with jitter.

    Args:
        retry_number: One-based retry number.
        server_delay: Optional minimum server-requested delay.

    Returns:
        Seconds to wait.
    """
    exponential_delay = 2 ** (retry_number - 1)
    delay = exponential_delay + random.uniform(0.0, exponential_delay * 0.25)
    return delay if server_delay is None else max(delay, server_delay)


def pmc_sleep_before_retry(
    retry_number: int,
    reason: str,
    response: requests.Response | None = None,
) -> None:
    """Report and perform one retry delay.

    Args:
        retry_number: One-based retry number.
        reason: Credential-safe diagnostic message.
        response: Optional response containing Retry-After.
    """
    server_delay = pmc_retry_after_seconds(response) if response is not None else None
    delay = pmc_retry_delay(retry_number, server_delay)
    print(f"  {reason}; retrying in {delay:.1f} seconds", file=sys.stderr)
    time.sleep(delay)


def pmc_request_batch(
    session: requests.Session,
    pmids: Sequence[str],
    tool_name: str,
    email: str | None,
    max_retries: int,
    statistics: pmc_RequestStatistics,
) -> list[pmc_PmcRecord]:
    """Retrieve one PMC ID Converter batch with bounded retries.

    Args:
        session: Persistent HTTP session.
        pmids: Batch of at most 200 PMIDs.
        tool_name: NCBI tool identifier.
        email: Optional NCBI contact email.
        max_retries: Retries after the initial request.
        statistics: Mutable request counters.

    Returns:
        Parsed records for every requested PMID.

    Raises:
        pmc_FatalApiError: If PMC rejects request configuration.
        pmc_BatchRequestError: If retries are exhausted.
    """
    params = {
        "ids": ",".join(pmids),
        "idtype": "pmid",
        "format": "json",
        "tool": tool_name,
    }
    if email:
        params["email"] = email
    total_attempts = max_retries + 1

    for attempt_index in range(total_attempts):
        try:
            statistics.http_requests += 1
            response = session.get(
                pmc_PMC_ID_CONVERTER_URL,
                params=params,
                timeout=(
                    pmc_DEFAULT_CONNECT_TIMEOUT_SECONDS,
                    pmc_DEFAULT_READ_TIMEOUT_SECONDS,
                ),
            )
        except (requests.ConnectionError, requests.Timeout):
            if attempt_index >= max_retries:
                raise pmc_BatchRequestError(
                    f"network failure after {total_attempts} attempts"
                ) from None
            pmc_sleep_before_retry(attempt_index + 1, "Network error")
            continue
        except requests.RequestException:
            raise pmc_BatchRequestError("non-retryable HTTP client error") from None

        if response.status_code in pmc_FATAL_HTTP_STATUSES:
            raise pmc_FatalApiError(
                f"PMC rejected the request with HTTP {response.status_code}"
            )
        if response.status_code in pmc_TRANSIENT_HTTP_STATUSES:
            if attempt_index >= max_retries:
                raise pmc_BatchRequestError(
                    f"HTTP {response.status_code} after {total_attempts} attempts"
                )
            pmc_sleep_before_retry(
                attempt_index + 1,
                f"PMC returned HTTP {response.status_code}",
                response,
            )
            continue
        if response.status_code != requests.codes.ok:
            raise pmc_BatchRequestError(
                f"PMC returned non-retryable HTTP {response.status_code}"
            )

        try:
            parsed_records = pmc_parse_pmc_results(response.json(), pmids)
        except (requests.JSONDecodeError, pmc_ResponseFormatError) as error:
            if attempt_index >= max_retries:
                raise pmc_BatchRequestError(
                    f"invalid PMC response after {total_attempts} attempts: {error}"
                ) from None
            pmc_sleep_before_retry(attempt_index + 1, "Invalid PMC response")
            continue

        statistics.successful_lookup_requests += 1
        return parsed_records

    raise pmc_BatchRequestError("request attempts exhausted")


def pmc_upsert_batch(
    connection: sqlite3.Connection,
    records: Sequence[pmc_PmcRecord],
) -> None:
    """Atomically commit one successful PMC batch.

    Args:
        connection: Open SQLite connection.
        records: Completed PMC records.
    """
    with connection:
        connection.executemany(
            """
            INSERT INTO pmc_cache (
                pmid,
                pmcid,
                pmc_fulltext_available,
                release_date,
                doi,
                checked_at
            )
            VALUES (?, ?, ?, ?, ?, ?)
            ON CONFLICT(pmid) DO UPDATE SET
                pmcid = excluded.pmcid,
                pmc_fulltext_available = excluded.pmc_fulltext_available,
                release_date = excluded.release_date,
                doi = excluded.doi,
                checked_at = excluded.checked_at
            """,
            [
                (
                    record.pmid,
                    record.pmcid,
                    record.pmc_fulltext_available,
                    record.release_date,
                    record.doi,
                    record.checked_at,
                )
                for record in records
            ],
        )


def pmc_chunked(values: Sequence[str], size: int) -> list[Sequence[str]]:
    """Partition values into maximum-size ordered batches.

    Args:
        values: Ordered values.
        size: Maximum batch size.

    Returns:
        Nonempty sequence slices.
    """
    return [values[index : index + size] for index in range(0, len(values), size)]


def pmc_retrieve_pmids(
    connection: sqlite3.Connection,
    pending_pmids: Sequence[str],
    args: argparse.Namespace,
    email: str | None,
    statistics: pmc_RequestStatistics,
) -> int:
    """Retrieve pending PMIDs sequentially and commit every batch.

    Args:
        connection: Open SQLite connection.
        pending_pmids: Ordered PMIDs requiring lookup.
        args: Parsed CLI arguments.
        email: Optional NCBI contact email.
        statistics: Mutable request counters.

    Returns:
        Number of failed batches.

    Raises:
        pmc_FatalApiError: If PMC rejects configuration.
    """
    batches = pmc_chunked(pending_pmids, args.batch_size)
    failed_batches = 0
    committed = 0

    with requests.Session() as session:
        session.headers.update(
            {
                "Accept": "application/json",
                "User-Agent": f"{args.tool}/1.0",
            }
        )
        for batch_number, pmid_batch in enumerate(batches, start=1):
            try:
                records = pmc_request_batch(
                    session,
                    pmid_batch,
                    args.tool,
                    email,
                    args.max_retries,
                    statistics,
                )
            except pmc_BatchRequestError as error:
                failed_batches += 1
                print(
                    f"Batch {batch_number}/{len(batches)} failed: {error}. "
                    f"Its {len(pmid_batch)} PMIDs remain uncached.",
                    file=sys.stderr,
                )
                continue

            pmc_upsert_batch(connection, records)
            pmcid_count = sum(record.pmcid is not None for record in records)
            live_count = sum(record.pmc_fulltext_available for record in records)
            committed += len(records)
            print(
                f"Batch {batch_number}/{len(batches)}: {len(records)} checked, "
                f"{pmcid_count} PMCIDs, {live_count} live"
            )
            if batch_number % 10 == 0 or batch_number == len(batches):
                print(f"Processed: {committed}/{len(pending_pmids)} PMIDs")
            if batch_number != len(batches) and args.delay:
                time.sleep(args.delay)
    return failed_batches


def pmc_input_summary(connection: sqlite3.Connection) -> tuple[int, int, int, int]:
    """Count checked, PMCID, live, and unchecked input records.

    Args:
        connection: Open SQLite connection with input table loaded.

    Returns:
        Counts in checked, PMCID, live, and unchecked order.
    """
    row = connection.execute(
        """
        SELECT
            COALESCE(SUM(pmc_cache.pmid IS NOT NULL), 0),
            COALESCE(SUM(pmc_cache.pmcid IS NOT NULL), 0),
            COALESCE(SUM(pmc_cache.pmc_fulltext_available = 1), 0),
            COALESCE(SUM(pmc_cache.pmid IS NULL), 0)
        FROM input_pmids
        LEFT JOIN pmc_cache ON pmc_cache.pmid = input_pmids.pmid
        """
    ).fetchone()
    if row is None:
        return 0, 0, 0, 0
    return tuple(int(value) for value in row)


def pmc_main(argv: Sequence[str] | None = None) -> int:
    """Run the durable PMC ID Converter workflow.

    Args:
        argv: Optional command-line argument sequence.

    Returns:
        Process exit status.
    """
    args = pmc_parse_arguments(argv)
    load_local_environment()
    try:
        total_input_pmids, unique_pmids = pmc_read_pmids(args.pmid_files)
        connection = pmc_connect_database(args.db)
    except (pmc_InputError, OSError, sqlite3.Error) as error:
        print(f"Error: {error}", file=sys.stderr)
        return 2

    statistics = pmc_RequestStatistics()
    failed_batches = 0
    interrupted = False
    fatal_error: pmc_FatalApiError | None = None
    try:
        pmc_load_input_table(connection, unique_pmids)
        pending_pmids = pmc_select_pmids_to_query(connection, args.refresh_all)
        reused = len(unique_pmids) - len(pending_pmids)
        planned_calls = math.ceil(len(pending_pmids) / args.batch_size)
        email = args.email or os.environ.get("NCBI_EMAIL")

        print(f"Input PMID rows: {total_input_pmids:,}")
        print(f"Unique PMIDs: {len(unique_pmids):,}")
        print(f"Already checked: {reused:,}")
        print(f"Remaining: {len(pending_pmids):,}")
        print(f"Planned PMC calls: {planned_calls:,}")

        if pending_pmids:
            try:
                failed_batches = pmc_retrieve_pmids(
                    connection,
                    pending_pmids,
                    args,
                    email,
                    statistics,
                )
            except pmc_FatalApiError as error:
                fatal_error = error

        checked, pmcid_count, live_count, unchecked = pmc_input_summary(connection)
        print("\nRun summary")
        print(f"Unique PMIDs: {len(unique_pmids):,}")
        print(f"PMC checks completed: {checked:,}")
        print(f"PMCIDs found: {pmcid_count:,}")
        print(f"Live PMC full text: {live_count:,}")
        if unchecked:
            print(f"Unchecked after this run: {unchecked:,}")
        print(f"HTTP calls made this run: {statistics.http_requests:,}")
        print(
            "Successful lookup calls this run: "
            f"{statistics.successful_lookup_requests:,}"
        )
        print(f"Cached checks reused: {reused:,}")
        print(f"SQLite cache path: {args.db.resolve()}")
    except KeyboardInterrupt:
        interrupted = True
        print(
            "\nInterrupted. Completed PMC batches are saved in SQLite.",
            file=sys.stderr,
        )
    except sqlite3.Error as error:
        print(f"Error: {error}", file=sys.stderr)
        return 2
    finally:
        connection.close()

    if interrupted:
        return 130
    if fatal_error is not None:
        print(f"Fatal PMC API error: {fatal_error}", file=sys.stderr)
        return 2
    if failed_batches:
        print(
            f"Completed with {failed_batches} failed batch(es); their PMIDs remain "
            "uncached for the next run.",
            file=sys.stderr,
        )
        return 1
    return 0


# OPENALEX AND SCIMAGO IMPLEMENTATION

OPENALEX_WORKS_URL = "https://api.openalex.org/works"
DEFAULT_DATABASE_PATH = Path("openalex_cache.sqlite")
DEFAULT_OUTPUT_PATH = Path("openalex_citations.csv")
DEFAULT_BATCH_SIZE = 100
MAX_BATCH_SIZE = 100
DEFAULT_MAX_RETRIES = 6
DEFAULT_CONNECT_TIMEOUT_SECONDS = 10.0
DEFAULT_READ_TIMEOUT_SECONDS = 60.0
TRANSIENT_HTTP_STATUSES = {408, 429, 500, 502, 503, 504}
FATAL_HTTP_STATUSES = {400, 401, 403, 404, 422}
PMID_PATTERN = re.compile(r"^[0-9]+$")
PMID_URL_PATTERN = re.compile(r"(?:^|/)([0-9]+)/?(?:[?#].*)?$")
ISSN_PATTERN = re.compile(r"^[0-9]{7}[0-9X]$")
ISSN_SEPARATOR = "|"
WORK_SELECT_FIELDS = (
    "id,ids,title,publication_year,publication_date,type,study_designs,doi,"
    "cited_by_count,open_access,best_oa_location,primary_location"
)
ARTICLE_CSV_COLUMNS = (
    "pmid",
    "pmcid",
    "pmc_fulltext_available",
    "pmc_release_date",
    "pmc_doi",
    "openalex_id",
    "open_access.is_oa",
    "best_oa_location",
    "title",
    "publication_year",
    "publication_date",
    "doi",
    "work_type",
    "study_designs",
    "cited_by_count",
    "source_openalex_id",
    "journal_name",
    "source_type",
    "issn_l",
    "issns",
    "status",
    "retrieved_at",
)
SCIMAGO_OUTPUT_COLUMNS = (
    "scimago_match_status",
    "scimago_matched_issns",
    "scimago_match_count",
    "scimago_rank",
    "scimago_source_id",
    "scimago_title",
    "scimago_type",
    "scimago_issns",
    "scimago_publisher",
    "scimago_sjr",
    "scimago_best_quartile",
    "scimago_h_index",
    "scimago_country",
    "scimago_region",
    "scimago_coverage",
    "scimago_categories",
    "scimago_areas",
)


class InputError(Exception):
    """Raised when the input PMID file is invalid."""


class FatalApiError(Exception):
    """Raised when further requests cannot succeed without intervention."""


class BatchRequestError(Exception):
    """Raised when one batch is exhausted or cannot be processed safely."""


class ResponseFormatError(Exception):
    """Raised when an OpenAlex response cannot be safely interpreted."""


@dataclass
class RequestStatistics:
    """Track HTTP attempts and successful lookup requests for one run."""

    http_requests: int = 0
    successful_lookup_requests: int = 0


@dataclass(frozen=True)
class CacheRecord:
    """Represent one durable article-metadata cache row."""

    pmid: str
    openalex_id: str | None
    is_oa: bool | None
    best_oa_location: str | None
    title: str | None
    publication_year: int | None
    publication_date: str | None
    doi: str | None
    work_type: str | None
    study_designs: str | None
    cited_by_count: int | None
    source_openalex_id: str | None
    journal_name: str | None
    source_type: str | None
    issn_l: str | None
    issns: str | None
    status: str
    retrieved_at: str
    metadata_complete: int
    study_designs_complete: int
    oa_metadata_complete: int
    error: str | None = None


@dataclass(frozen=True)
class WorkMetadata:
    """Represent validated fields extracted from one OpenAlex Work."""

    pmid: str
    openalex_id: str
    is_oa: bool
    best_oa_location: str | None
    title: str | None
    publication_year: int | None
    publication_date: str | None
    doi: str | None
    work_type: str | None
    study_designs: str | None
    cited_by_count: int
    source_openalex_id: str | None
    journal_name: str | None
    source_type: str | None
    issn_l: str | None
    issns: str | None


@dataclass(frozen=True)
class CacheStateCounts:
    """Count input PMIDs by cache-completeness state."""

    fully_cached: int
    oa_metadata_pending: int
    study_design_pending: int
    citation_only: int
    completely_uncached: int


@dataclass(frozen=True)
class ScimagoRecord:
    """Represent one SCImago source row and its normalized ISSNs."""

    position: int
    normalized_issns: frozenset[str]
    values: dict[str, str]


@dataclass(frozen=True)
class ScimagoIndex:
    """Store SCImago rows and an ISSN-to-row lookup index."""

    records: tuple[ScimagoRecord, ...]
    by_issn: dict[str, tuple[int, ...]]


def parse_arguments(argv: Sequence[str] | None = None) -> argparse.Namespace:
    """Parse and validate command-line options.

    Args:
        argv: Optional argument sequence. Uses ``sys.argv`` when omitted.

    Returns:
        Parsed command-line arguments.
    """
    parser = argparse.ArgumentParser(
        description=(
            "Retrieve OpenAlex citation and publication metadata for PMIDs using "
            "durable SQLite caching and batches of up to 100 IDs."
        )
    )
    parser.add_argument("pmid_file", type=Path, help="Text file with one PMID per line")
    parser.add_argument(
        "--db",
        type=Path,
        default=DEFAULT_DATABASE_PATH,
        help=f"SQLite cache path (default: {DEFAULT_DATABASE_PATH})",
    )
    parser.add_argument(
        "--output",
        type=Path,
        default=DEFAULT_OUTPUT_PATH,
        help=f"Output CSV path (default: {DEFAULT_OUTPUT_PATH})",
    )
    parser.add_argument(
        "--scimago",
        type=Path,
        help=(
            "Optional semicolon-delimited SCImago CSV; matched fields are appended "
            "to the output using ISSNs"
        ),
    )
    refresh_group = parser.add_mutually_exclusive_group()
    refresh_group.add_argument(
        "--refresh-days",
        type=nonnegative_integer,
        metavar="N",
        help="Re-query found records older than N days",
    )
    refresh_group.add_argument(
        "--refresh-all",
        action="store_true",
        help="Re-query every input PMID, including not-found records",
    )
    parser.add_argument(
        "--batch-size",
        type=batch_size,
        default=DEFAULT_BATCH_SIZE,
        help=f"PMIDs per request, 1-{MAX_BATCH_SIZE} (default: 100)",
    )
    parser.add_argument(
        "--max-retries",
        type=nonnegative_integer,
        default=DEFAULT_MAX_RETRIES,
        help="Retries after the initial request (default: 6)",
    )
    return parser.parse_args(argv)


def nonnegative_integer(value: str) -> int:
    """Convert a CLI value to a nonnegative integer.

    Args:
        value: Raw argument value.

    Returns:
        The validated integer.

    Raises:
        argparse.ArgumentTypeError: If the value is not a nonnegative integer.
    """
    try:
        parsed_value = int(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError("must be an integer") from error
    if parsed_value < 0:
        raise argparse.ArgumentTypeError("must be zero or greater")
    return parsed_value


def batch_size(value: str) -> int:
    """Validate an OpenAlex OR-filter batch size.

    Args:
        value: Raw argument value.

    Returns:
        A batch size from 1 through 100.

    Raises:
        argparse.ArgumentTypeError: If the value is outside the accepted range.
    """
    parsed_value = nonnegative_integer(value)
    if not 1 <= parsed_value <= MAX_BATCH_SIZE:
        raise argparse.ArgumentTypeError(f"must be between 1 and {MAX_BATCH_SIZE}")
    return parsed_value


def read_pmids(path: Path) -> tuple[int, list[str]]:
    """Read, validate, and order-deduplicate PMIDs.

    Blank lines are ignored. Every nonblank line must contain ASCII digits only.

    Args:
        path: Input text-file path.

    Returns:
        A pair containing the nonblank PMID count and ordered unique PMIDs.

    Raises:
        InputError: If the file cannot be read, is empty, or has invalid lines.
    """
    unique_pmids: list[str] = []
    seen_pmids: set[str] = set()
    invalid_lines: list[tuple[int, str]] = []
    invalid_count = 0
    total_pmids = 0

    try:
        with path.open("r", encoding="utf-8-sig") as input_file:
            for line_number, raw_line in enumerate(input_file, start=1):
                pmid = raw_line.strip()
                if not pmid:
                    continue
                total_pmids += 1
                if not PMID_PATTERN.fullmatch(pmid):
                    invalid_count += 1
                    if len(invalid_lines) < 10:
                        invalid_lines.append((line_number, pmid))
                    continue
                if pmid not in seen_pmids:
                    seen_pmids.add(pmid)
                    unique_pmids.append(pmid)
    except (OSError, UnicodeError) as error:
        raise InputError(f"Cannot read PMID file '{path}': {error}") from error

    if invalid_lines:
        examples = ", ".join(
            f"line {line_number}: {value!r}" for line_number, value in invalid_lines
        )
        suffix = " (showing first 10)" if invalid_count > 10 else ""
        raise InputError(f"Non-numeric PMID values found: {examples}{suffix}")
    if not unique_pmids:
        raise InputError(f"No PMIDs found in '{path}'")
    return total_pmids, unique_pmids


def connect_database(path: Path) -> sqlite3.Connection:
    """Open and initialize the durable SQLite citation cache.

    Args:
        path: Cache database path.

    Returns:
        An initialized SQLite connection.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    connection = sqlite3.connect(path, timeout=30.0)
    connection.execute("PRAGMA busy_timeout = 30000")
    connection.execute("PRAGMA journal_mode = WAL")
    connection.execute("PRAGMA synchronous = FULL")
    connection.execute(
        """
        CREATE TABLE IF NOT EXISTS pmid_cache (
            pmid TEXT PRIMARY KEY,
            openalex_id TEXT,
            is_oa INTEGER CHECK (is_oa IN (0, 1)),
            best_oa_location TEXT,
            title TEXT,
            publication_year INTEGER,
            publication_date TEXT,
            doi TEXT,
            work_type TEXT,
            study_designs TEXT,
            cited_by_count INTEGER,
            source_openalex_id TEXT,
            journal_name TEXT,
            source_type TEXT,
            issn_l TEXT,
            issns TEXT,
            status TEXT NOT NULL CHECK (status IN ('found', 'not_found')),
            retrieved_at TEXT NOT NULL,
            metadata_complete INTEGER NOT NULL DEFAULT 0,
            study_designs_complete INTEGER NOT NULL DEFAULT 0,
            oa_metadata_complete INTEGER NOT NULL DEFAULT 0,
            error TEXT
        )
        """
    )
    migrate_cache_schema(connection)
    connection.execute(
        """
        CREATE INDEX IF NOT EXISTS idx_pmid_cache_status_retrieved
        ON pmid_cache (status, retrieved_at)
        """
    )
    connection.commit()
    return connection


def migrate_cache_schema(connection: sqlite3.Connection) -> None:
    """Add metadata columns without replacing existing citation records.

    Args:
        connection: Open SQLite cache connection.
    """
    existing_columns = {
        row[1] for row in connection.execute("PRAGMA table_info(pmid_cache)")
    }
    column_definitions = {
        "is_oa": "INTEGER CHECK (is_oa IN (0, 1))",
        "best_oa_location": "TEXT",
        "title": "TEXT",
        "publication_year": "INTEGER",
        "publication_date": "TEXT",
        "doi": "TEXT",
        "work_type": "TEXT",
        "study_designs": "TEXT",
        "source_openalex_id": "TEXT",
        "journal_name": "TEXT",
        "source_type": "TEXT",
        "issn_l": "TEXT",
        "issns": "TEXT",
        "metadata_complete": "INTEGER NOT NULL DEFAULT 0",
        "study_designs_complete": "INTEGER NOT NULL DEFAULT 0",
        "oa_metadata_complete": "INTEGER NOT NULL DEFAULT 0",
    }
    with connection:
        for column_name, column_definition in column_definitions.items():
            if column_name not in existing_columns:
                connection.execute(
                    f"ALTER TABLE pmid_cache ADD COLUMN {column_name} "
                    f"{column_definition}"
                )
        connection.execute(
            """
            CREATE TABLE IF NOT EXISTS sources (
                source_openalex_id TEXT PRIMARY KEY,
                display_name TEXT,
                source_type TEXT,
                issn_l TEXT,
                issns TEXT
            )
            """
        )
        connection.execute(
            """
            CREATE TABLE IF NOT EXISTS pmc_cache (
                pmid TEXT PRIMARY KEY,
                pmcid TEXT,
                pmc_fulltext_available INTEGER NOT NULL
                    CHECK (pmc_fulltext_available IN (0, 1)),
                release_date TEXT,
                doi TEXT,
                checked_at TEXT NOT NULL
            )
            """
        )


def load_input_table(
    connection: sqlite3.Connection,
    pmids: Sequence[str],
) -> None:
    """Load ordered input PMIDs into a connection-local temporary table.

    Args:
        connection: Open cache connection.
        pmids: Ordered unique PMIDs.
    """
    with connection:
        connection.execute("DROP TABLE IF EXISTS temp.input_pmids")
        connection.execute(
            """
            CREATE TEMPORARY TABLE input_pmids (
                position INTEGER PRIMARY KEY,
                pmid TEXT NOT NULL UNIQUE
            )
            """
        )
        connection.executemany(
            "INSERT INTO input_pmids (position, pmid) VALUES (?, ?)",
            enumerate(pmids),
        )


def utc_timestamp() -> str:
    """Return the current UTC time in stable second-resolution ISO format.

    Returns:
        A timestamp ending in ``Z``.
    """
    return datetime.now(UTC).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def refresh_cutoff(refresh_days: int) -> str:
    """Calculate the UTC timestamp before which found rows are stale.

    Args:
        refresh_days: Maximum accepted record age in whole days.

    Returns:
        The exclusive freshness cutoff in cache timestamp format.
    """
    cutoff = datetime.now(UTC) - timedelta(days=refresh_days)
    return cutoff.replace(microsecond=0).isoformat().replace("+00:00", "Z")


def select_pmids_to_query(
    connection: sqlite3.Connection,
    refresh_days: int | None,
    refresh_all: bool,
) -> list[str]:
    """Select uncached, metadata-incomplete, or stale PMIDs in input order.

    Args:
        connection: Open cache connection with ``input_pmids`` loaded.
        refresh_days: Optional maximum age for found records.
        refresh_all: Whether every input record must be refreshed.

    Returns:
        Ordered PMIDs to query from OpenAlex.
    """
    if refresh_all:
        rows = connection.execute("SELECT pmid FROM input_pmids ORDER BY position")
    elif refresh_days is not None:
        cutoff = refresh_cutoff(refresh_days)
        rows = connection.execute(
            """
            SELECT input_pmids.pmid
            FROM input_pmids
            LEFT JOIN pmid_cache
                ON pmid_cache.pmid = input_pmids.pmid
                AND pmid_cache.status IN ('found', 'not_found')
            WHERE pmid_cache.pmid IS NULL
                OR pmid_cache.metadata_complete != 1
                OR pmid_cache.study_designs_complete != 1
                OR pmid_cache.oa_metadata_complete != 1
                OR (
                    pmid_cache.status = 'found'
                    AND pmid_cache.retrieved_at < ?
                )
            ORDER BY input_pmids.position
            """,
            (cutoff,),
        )
    else:
        rows = connection.execute(
            """
            SELECT input_pmids.pmid
            FROM input_pmids
            LEFT JOIN pmid_cache
                ON pmid_cache.pmid = input_pmids.pmid
                AND pmid_cache.status IN ('found', 'not_found')
            WHERE pmid_cache.pmid IS NULL
                OR pmid_cache.metadata_complete != 1
                OR pmid_cache.study_designs_complete != 1
                OR pmid_cache.oa_metadata_complete != 1
            ORDER BY input_pmids.position
            """
        )
    return [row[0] for row in rows]


def normalize_pmid(value: Any) -> str | None:
    """Normalize an OpenAlex PMID identifier to digits only.

    Args:
        value: Value from ``work.ids.pmid``.

    Returns:
        A numeric PMID string, or ``None`` when normalization is unsafe.
    """
    if not isinstance(value, str):
        return None
    stripped_value = value.strip()
    if PMID_PATTERN.fullmatch(stripped_value):
        return stripped_value
    match = PMID_URL_PATTERN.search(stripped_value)
    return match.group(1) if match else None


def normalize_openalex_id(value: Any) -> str | None:
    """Normalize an OpenAlex work URL to its compact work identifier.

    Args:
        value: OpenAlex ``id`` response value.

    Returns:
        A compact identifier such as ``W123``, or ``None`` if invalid.
    """
    if not isinstance(value, str):
        return None
    normalized_value = value.strip().rstrip("/").rsplit("/", maxsplit=1)[-1]
    return normalized_value or None


def optional_text(value: Any) -> str | None:
    """Return stripped nonempty text while preserving missing values as null.

    Args:
        value: Potential string value.

    Returns:
        Stripped text, or ``None`` for null, non-string, or empty input.
    """
    if not isinstance(value, str):
        return None
    stripped_value = value.strip()
    return stripped_value or None


def normalize_issn(value: Any) -> str | None:
    """Normalize a syntactically valid ISSN for cross-dataset matching.

    Args:
        value: ISSN in formatted or unformatted form.

    Returns:
        Eight uppercase characters without punctuation, or ``None`` if invalid.
    """
    if not isinstance(value, str):
        return None
    uppercase_value = value.strip().upper()
    if re.search(r"[A-WY-Z]", uppercase_value):
        return None
    normalized_value = re.sub(r"[^0-9X]", "", uppercase_value)
    if not ISSN_PATTERN.fullmatch(normalized_value):
        return None
    return normalized_value


def serialize_issns(value: Any) -> str | None:
    """Serialize every nonempty OpenAlex ISSN using a stable delimiter.

    Args:
        value: OpenAlex source ``issn`` list.

    Returns:
        Pipe-delimited original ISSNs, or ``None`` when none are supplied.
    """
    if not isinstance(value, list):
        return None
    issns = [issn.strip() for issn in value if isinstance(issn, str) and issn.strip()]
    return ISSN_SEPARATOR.join(issns) if issns else None


def serialize_study_designs(value: Any) -> str | None:
    """Serialize OpenAlex study-design labels with a vertical-bar delimiter.

    OpenAlex currently returns a list of study-design objects. String values
    are also accepted defensively, and object labels fall back to their IDs.

    Args:
        value: OpenAlex ``study_designs`` value.

    Returns:
        Ordered, deduplicated labels joined by ``|``, or ``None``.
    """
    if value is None:
        return None
    items = value if isinstance(value, list) else [value]
    labels: list[str] = []
    seen_labels: set[str] = set()
    for item in items:
        label: str | None
        if isinstance(item, dict):
            label = optional_text(item.get("display_name")) or optional_text(
                item.get("id")
            )
        else:
            label = optional_text(item)
        if label is not None and label not in seen_labels:
            seen_labels.add(label)
            labels.append(label)
    return ISSN_SEPARATOR.join(labels) if labels else None


def optional_publication_year(value: Any) -> int | None:
    """Validate an optional OpenAlex publication year.

    Args:
        value: OpenAlex ``publication_year`` value.

    Returns:
        Integer year when valid, otherwise ``None``.
    """
    if isinstance(value, bool) or not isinstance(value, int):
        return None
    return value


def serialize_json_object(value: Any, field_name: str) -> str | None:
    """Serialize an optional OpenAlex object as compact deterministic JSON.

    Args:
        value: Parsed OpenAlex field value.
        field_name: Field name used in validation errors.

    Returns:
        Compact JSON when the value is an object, otherwise ``None`` for null.

    Raises:
        ResponseFormatError: If a non-null value is not an object.
    """
    if value is None:
        return None
    if not isinstance(value, dict):
        raise ResponseFormatError(f"a work has an invalid {field_name} object")
    return json.dumps(
        value,
        ensure_ascii=True,
        sort_keys=True,
        separators=(",", ":"),
    )


def parse_results(
    payload: Any,
    requested_pmids: Sequence[str],
) -> dict[str, WorkMetadata]:
    """Validate and map an OpenAlex response without relying on result order.

    Args:
        payload: Parsed response JSON.
        requested_pmids: PMIDs included in the batch filter.

    Returns:
        Mapping from returned requested PMID to article and source metadata.

    Raises:
        ResponseFormatError: If the response could cause unsafe cache entries.
    """
    if not isinstance(payload, dict) or not isinstance(payload.get("results"), list):
        raise ResponseFormatError("response does not contain a results list")

    requested_set = set(requested_pmids)
    found_records: dict[str, WorkMetadata] = {}
    for work in payload["results"]:
        if not isinstance(work, dict) or not isinstance(work.get("ids"), dict):
            raise ResponseFormatError("a work is missing its IDs mapping")
        pmid = normalize_pmid(work["ids"].get("pmid"))
        openalex_id = normalize_openalex_id(work.get("id"))
        cited_by_count = work.get("cited_by_count")
        if pmid is None or openalex_id is None:
            raise ResponseFormatError("a work has an invalid PMID or OpenAlex ID")
        if pmid not in requested_set:
            raise ResponseFormatError("response contains an unrequested PMID")
        if (
            isinstance(cited_by_count, bool)
            or not isinstance(cited_by_count, int)
            or cited_by_count < 0
        ):
            raise ResponseFormatError("a work has an invalid cited_by_count")
        if pmid in found_records:
            raise ResponseFormatError("response contains duplicate PMID results")

        open_access = work.get("open_access")
        if not isinstance(open_access, dict) or not isinstance(
            open_access.get("is_oa"), bool
        ):
            raise ResponseFormatError("a work has an invalid open_access.is_oa value")

        primary_location = work.get("primary_location")
        source: dict[str, Any] | None = None
        if isinstance(primary_location, dict) and isinstance(
            primary_location.get("source"), dict
        ):
            source = primary_location["source"]

        found_records[pmid] = WorkMetadata(
            pmid=pmid,
            openalex_id=openalex_id,
            is_oa=open_access["is_oa"],
            best_oa_location=serialize_json_object(
                work.get("best_oa_location"),
                "best_oa_location",
            ),
            title=optional_text(work.get("title")),
            publication_year=optional_publication_year(work.get("publication_year")),
            publication_date=optional_text(work.get("publication_date")),
            doi=optional_text(work.get("doi")),
            work_type=optional_text(work.get("type")),
            study_designs=serialize_study_designs(work.get("study_designs")),
            cited_by_count=cited_by_count,
            source_openalex_id=(
                normalize_openalex_id(source.get("id")) if source else None
            ),
            journal_name=optional_text(source.get("display_name")) if source else None,
            source_type=optional_text(source.get("type")) if source else None,
            issn_l=optional_text(source.get("issn_l")) if source else None,
            issns=serialize_issns(source.get("issn")) if source else None,
        )
    return found_records


def retry_after_seconds(response: requests.Response) -> float | None:
    """Parse an HTTP Retry-After header as a nonnegative delay.

    Args:
        response: HTTP response carrying optional rate-limit headers.

    Returns:
        Delay in seconds, or ``None`` when the header is absent or invalid.
    """
    header_value = response.headers.get("Retry-After")
    if not header_value:
        return None
    try:
        return max(0.0, float(header_value))
    except ValueError:
        try:
            retry_time = email.utils.parsedate_to_datetime(header_value)
        except (TypeError, ValueError, OverflowError):
            return None
        if retry_time.tzinfo is None:
            retry_time = retry_time.replace(tzinfo=UTC)
        return max(0.0, (retry_time - datetime.now(UTC)).total_seconds())


def retry_delay(retry_number: int, server_delay: float | None = None) -> float:
    """Calculate exponential backoff with jitter and Retry-After support.

    Args:
        retry_number: One-based retry number.
        server_delay: Optional minimum delay requested by the server.

    Returns:
        Number of seconds to wait before the retry.
    """
    exponential_delay = 2 ** (retry_number - 1)
    jittered_delay = exponential_delay + random.uniform(0.0, exponential_delay * 0.25)
    if server_delay is None:
        return jittered_delay
    return max(jittered_delay, server_delay)


def rate_limit_remaining(response: requests.Response) -> str | None:
    """Read a remaining-quota header from a successful OpenAlex response.

    Args:
        response: Successful HTTP response.

    Returns:
        Header value when supplied by the server.
    """
    for header_name in (
        "X-RateLimit-Remaining",
        "RateLimit-Remaining",
        "X-Rate-Limit-Remaining",
    ):
        header_value = response.headers.get(header_name)
        if header_value is not None:
            return header_value
    return None


def sleep_before_retry(
    retry_number: int,
    reason: str,
    response: requests.Response | None = None,
) -> None:
    """Report and perform one interruptible retry delay.

    Args:
        retry_number: One-based retry number.
        reason: Safe diagnostic that never contains credentials.
        response: Optional HTTP response with a Retry-After header.
    """
    server_delay = retry_after_seconds(response) if response is not None else None
    delay = retry_delay(retry_number, server_delay)
    print(f"  {reason}; retrying in {delay:.1f} seconds", file=sys.stderr)
    time.sleep(delay)


def request_batch(
    session: requests.Session,
    api_key: str,
    pmids: Sequence[str],
    max_retries: int,
    statistics: RequestStatistics,
) -> tuple[dict[str, WorkMetadata], str | None]:
    """Request and validate one PMID batch with bounded transient retries.

    Args:
        session: Persistent HTTP session.
        api_key: OpenAlex API key, used only as a request parameter.
        pmids: One batch of at most 100 PMIDs.
        max_retries: Retries permitted after the first attempt.
        statistics: Mutable per-run request counters.

    Returns:
        Parsed found-record mapping and optional remaining-quota value.

    Raises:
        FatalApiError: If credentials or request configuration are rejected.
        BatchRequestError: If retryable attempts are exhausted or an isolated
            non-transient request fails.
    """
    params = {
        "filter": f"pmid:{'|'.join(pmids)}",
        "per_page": str(MAX_BATCH_SIZE),
        "select": WORK_SELECT_FIELDS,
        "api_key": api_key,
    }
    total_attempts = max_retries + 1

    for attempt_index in range(total_attempts):
        try:
            statistics.http_requests += 1
            response = session.get(
                OPENALEX_WORKS_URL,
                params=params,
                timeout=(
                    DEFAULT_CONNECT_TIMEOUT_SECONDS,
                    DEFAULT_READ_TIMEOUT_SECONDS,
                ),
            )
        except (requests.ConnectionError, requests.Timeout):
            if attempt_index >= max_retries:
                raise BatchRequestError(
                    f"network failure after {total_attempts} attempts"
                ) from None
            sleep_before_retry(attempt_index + 1, "Network error")
            continue
        except requests.RequestException:
            raise BatchRequestError("non-retryable HTTP client error") from None

        if response.status_code in FATAL_HTTP_STATUSES:
            raise FatalApiError(
                f"OpenAlex rejected the request with HTTP {response.status_code}; "
                "check OPENALEX_API_KEY and request configuration"
            )
        if response.status_code in TRANSIENT_HTTP_STATUSES:
            if attempt_index >= max_retries:
                raise BatchRequestError(
                    f"HTTP {response.status_code} after {total_attempts} attempts"
                )
            sleep_before_retry(
                attempt_index + 1,
                f"OpenAlex returned HTTP {response.status_code}",
                response,
            )
            continue
        if response.status_code != requests.codes.ok:
            raise BatchRequestError(
                f"OpenAlex returned non-retryable HTTP {response.status_code}"
            )

        try:
            found_records = parse_results(response.json(), pmids)
        except (requests.JSONDecodeError, ResponseFormatError) as error:
            if attempt_index >= max_retries:
                raise BatchRequestError(
                    f"invalid OpenAlex response after {total_attempts} attempts: {error}"
                ) from None
            sleep_before_retry(attempt_index + 1, "Invalid OpenAlex response")
            continue

        statistics.successful_lookup_requests += 1
        return found_records, rate_limit_remaining(response)

    raise BatchRequestError("request attempts exhausted")


def records_for_batch(
    requested_pmids: Sequence[str],
    found_records: dict[str, WorkMetadata],
) -> list[CacheRecord]:
    """Build found and not-found cache rows for one successful response.

    Args:
        requested_pmids: PMIDs included in the successful request.
        found_records: Validated results keyed by PMID.

    Returns:
        One cache record for every requested PMID.
    """
    retrieved_at = utc_timestamp()
    cache_records: list[CacheRecord] = []
    for pmid in requested_pmids:
        found_value = found_records.get(pmid)
        if found_value is None:
            cache_records.append(
                CacheRecord(
                    pmid=pmid,
                    openalex_id=None,
                    is_oa=None,
                    best_oa_location=None,
                    title=None,
                    publication_year=None,
                    publication_date=None,
                    doi=None,
                    work_type=None,
                    study_designs=None,
                    cited_by_count=None,
                    source_openalex_id=None,
                    journal_name=None,
                    source_type=None,
                    issn_l=None,
                    issns=None,
                    status="not_found",
                    retrieved_at=retrieved_at,
                    metadata_complete=1,
                    study_designs_complete=1,
                    oa_metadata_complete=1,
                )
            )
            continue
        cache_records.append(
            CacheRecord(
                pmid=pmid,
                openalex_id=found_value.openalex_id,
                is_oa=found_value.is_oa,
                best_oa_location=found_value.best_oa_location,
                title=found_value.title,
                publication_year=found_value.publication_year,
                publication_date=found_value.publication_date,
                doi=found_value.doi,
                work_type=found_value.work_type,
                study_designs=found_value.study_designs,
                cited_by_count=found_value.cited_by_count,
                source_openalex_id=found_value.source_openalex_id,
                journal_name=found_value.journal_name,
                source_type=found_value.source_type,
                issn_l=found_value.issn_l,
                issns=found_value.issns,
                status="found",
                retrieved_at=retrieved_at,
                metadata_complete=1,
                study_designs_complete=1,
                oa_metadata_complete=1,
            )
        )
    return cache_records


def upsert_batch(
    connection: sqlite3.Connection,
    records: Sequence[CacheRecord],
) -> None:
    """Atomically commit every record from one successful OpenAlex batch.

    Args:
        connection: Open cache connection.
        records: Complete found/not-found records for one batch.
    """
    values = [
        (
            record.pmid,
            record.openalex_id,
            record.is_oa,
            record.best_oa_location,
            record.title,
            record.publication_year,
            record.publication_date,
            record.doi,
            record.work_type,
            record.study_designs,
            record.cited_by_count,
            record.source_openalex_id,
            record.journal_name,
            record.source_type,
            record.issn_l,
            record.issns,
            record.status,
            record.retrieved_at,
            record.metadata_complete,
            record.study_designs_complete,
            record.oa_metadata_complete,
            record.error,
        )
        for record in records
    ]
    with connection:
        connection.executemany(
            """
            INSERT INTO pmid_cache (
                pmid,
                openalex_id,
                is_oa,
                best_oa_location,
                title,
                publication_year,
                publication_date,
                doi,
                work_type,
                study_designs,
                cited_by_count,
                source_openalex_id,
                journal_name,
                source_type,
                issn_l,
                issns,
                status,
                retrieved_at,
                metadata_complete,
                study_designs_complete,
                oa_metadata_complete,
                error
            )
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(pmid) DO UPDATE SET
                openalex_id = excluded.openalex_id,
                is_oa = excluded.is_oa,
                best_oa_location = excluded.best_oa_location,
                title = excluded.title,
                publication_year = excluded.publication_year,
                publication_date = excluded.publication_date,
                doi = excluded.doi,
                work_type = excluded.work_type,
                study_designs = excluded.study_designs,
                cited_by_count = excluded.cited_by_count,
                source_openalex_id = excluded.source_openalex_id,
                journal_name = excluded.journal_name,
                source_type = excluded.source_type,
                issn_l = excluded.issn_l,
                issns = excluded.issns,
                status = excluded.status,
                retrieved_at = excluded.retrieved_at,
                metadata_complete = excluded.metadata_complete,
                study_designs_complete = excluded.study_designs_complete,
                oa_metadata_complete = excluded.oa_metadata_complete,
                error = excluded.error
            """,
            values,
        )
        connection.executemany(
            """
            INSERT INTO sources (
                source_openalex_id,
                display_name,
                source_type,
                issn_l,
                issns
            )
            VALUES (?, ?, ?, ?, ?)
            ON CONFLICT(source_openalex_id) DO UPDATE SET
                display_name = excluded.display_name,
                source_type = excluded.source_type,
                issn_l = excluded.issn_l,
                issns = excluded.issns
            """,
            [
                (
                    record.source_openalex_id,
                    record.journal_name,
                    record.source_type,
                    record.issn_l,
                    record.issns,
                )
                for record in records
                if record.source_openalex_id is not None
            ],
        )


def chunked(values: Sequence[str], size: int) -> list[Sequence[str]]:
    """Partition an ordered sequence into full-size batches plus a remainder.

    Args:
        values: Values to partition.
        size: Maximum values in each batch.

    Returns:
        Ordered nonempty sequence slices.
    """
    return [values[index : index + size] for index in range(0, len(values), size)]


def retrieve_pending_pmids(
    connection: sqlite3.Connection,
    pending_pmids: Sequence[str],
    batch_size_value: int,
    max_retries: int,
    api_key: str,
    statistics: RequestStatistics,
) -> tuple[int, int]:
    """Retrieve pending PMIDs sequentially and commit each successful batch.

    Args:
        connection: Open cache connection.
        pending_pmids: Ordered uncached or stale PMIDs.
        batch_size_value: Maximum PMIDs per OR-filter request.
        max_retries: Retries permitted for each batch.
        api_key: OpenAlex API key.
        statistics: Mutable per-run request counters.

    Returns:
        Committed PMID count and failed batch count.

    Raises:
        FatalApiError: If OpenAlex rejects credentials or configuration.
    """
    batches = chunked(pending_pmids, batch_size_value)
    committed_pmids = 0
    failed_batches = 0

    with requests.Session() as session:
        session.headers.update(
            {
                "Accept": "application/json",
                "User-Agent": "openalex-citations/1.0",
            }
        )
        for batch_number, pmid_batch in enumerate(batches, start=1):
            try:
                found_records, remaining_quota = request_batch(
                    session,
                    api_key,
                    pmid_batch,
                    max_retries,
                    statistics,
                )
            except BatchRequestError as error:
                failed_batches += 1
                print(
                    f"Batch {batch_number}/{len(batches)} failed: {error}. "
                    f"Its {len(pmid_batch)} PMIDs remain uncached.",
                    file=sys.stderr,
                )
                continue

            cache_records = records_for_batch(pmid_batch, found_records)
            upsert_batch(connection, cache_records)
            found_count = len(found_records)
            not_found_count = len(pmid_batch) - found_count
            committed_pmids += len(pmid_batch)
            not_found_text = f", {not_found_count} not found" if not_found_count else ""
            quota_text = (
                f", rate limit remaining: {remaining_quota}"
                if remaining_quota is not None
                else ""
            )
            print(
                f"Batch {batch_number}/{len(batches)}: {len(pmid_batch)} requested, "
                f"{found_count} found{not_found_text}{quota_text}"
            )
            if batch_number % 10 == 0 or batch_number == len(batches):
                print(f"Processed: {committed_pmids}/{len(pending_pmids)} PMIDs")

    return committed_pmids, failed_batches


def normalized_article_issns(issn_l: Any, issns: Any) -> frozenset[str]:
    """Collect every normalized ISSN stored for an OpenAlex article.

    Args:
        issn_l: Original OpenAlex linking ISSN.
        issns: Pipe-delimited original OpenAlex ISSN list.

    Returns:
        Unique normalized article ISSNs.
    """
    values: list[Any] = [issn_l]
    if isinstance(issns, str):
        values.extend(issns.split(ISSN_SEPARATOR))
    return frozenset(
        normalized
        for value in values
        if (normalized := normalize_issn(value)) is not None
    )


def read_scimago_index(path: Path) -> ScimagoIndex:
    """Read a semicolon-delimited SCImago CSV and index rows by every ISSN.

    Args:
        path: SCImago CSV path.

    Returns:
        Immutable SCImago records plus an ISSN lookup index.

    Raises:
        InputError: If the file cannot be read or lacks required columns.
    """
    output_to_source_column = {
        "scimago_rank": "Rank",
        "scimago_source_id": "Sourceid",
        "scimago_title": "Title",
        "scimago_type": "Type",
        "scimago_issns": "Issn",
        "scimago_publisher": "Publisher",
        "scimago_sjr": "SJR",
        "scimago_best_quartile": "SJR Best Quartile",
        "scimago_h_index": "H index",
        "scimago_country": "Country",
        "scimago_region": "Region",
        "scimago_coverage": "Coverage",
        "scimago_categories": "Categories",
        "scimago_areas": "Areas",
    }
    records: list[ScimagoRecord] = []
    mutable_index: dict[str, list[int]] = {}

    try:
        with path.open("r", encoding="utf-8-sig", newline="") as input_file:
            reader = csv.reader(input_file, delimiter=";")
            try:
                header = next(reader)
            except StopIteration as error:
                raise InputError(f"SCImago CSV '{path}' is empty") from error

            missing_columns = sorted(
                set(output_to_source_column.values()).difference(header)
            )
            if missing_columns:
                raise InputError(
                    "SCImago CSV is missing required columns: "
                    f"{', '.join(missing_columns)}"
                )
            column_indexes = {
                output_name: header.index(source_name)
                for output_name, source_name in output_to_source_column.items()
            }

            for line_number, row in enumerate(reader, start=2):
                if len(row) != len(header):
                    raise InputError(
                        f"SCImago CSV line {line_number} has {len(row)} fields; "
                        f"expected {len(header)}"
                    )
                values = {
                    output_name: row[column_index].strip()
                    for output_name, column_index in column_indexes.items()
                }
                normalized_issns = frozenset(
                    normalized
                    for value in re.split(r"[,|;]", values["scimago_issns"])
                    if (normalized := normalize_issn(value)) is not None
                )
                position = len(records)
                records.append(
                    ScimagoRecord(
                        position=position,
                        normalized_issns=normalized_issns,
                        values=values,
                    )
                )
                for normalized_issn_value in normalized_issns:
                    mutable_index.setdefault(normalized_issn_value, []).append(position)
    except (OSError, UnicodeError, csv.Error) as error:
        raise InputError(f"Cannot read SCImago CSV '{path}': {error}") from error

    return ScimagoIndex(
        records=tuple(records),
        by_issn={key: tuple(value) for key, value in mutable_index.items()},
    )


def scimago_rank_sort_value(value: str) -> int:
    """Convert a SCImago rank to a deterministic sorting value.

    Args:
        value: Rank field from SCImago.

    Returns:
        Integer rank, or a large value when the field is not numeric.
    """
    try:
        return int(value)
    except ValueError:
        return sys.maxsize


def match_scimago(
    issn_l: Any,
    issns: Any,
    scimago_index: ScimagoIndex,
) -> list[Any]:
    """Match one article to SCImago using any normalized ISSN.

    Multiple candidates are flagged and resolved deterministically by greatest
    ISSN overlap, then lowest SCImago rank, then source-file order. Journal
    titles are never used for matching.

    Args:
        issn_l: Original OpenAlex linking ISSN.
        issns: Pipe-delimited original OpenAlex ISSNs.
        scimago_index: Preloaded SCImago ISSN index.

    Returns:
        Values aligned with ``SCIMAGO_OUTPUT_COLUMNS``.
    """
    article_issns = normalized_article_issns(issn_l, issns)
    if not article_issns:
        return [
            "no_article_issn",
            None,
            0,
            *([None] * (len(SCIMAGO_OUTPUT_COLUMNS) - 3)),
        ]

    candidate_positions = {
        position
        for article_issn in article_issns
        for position in scimago_index.by_issn.get(article_issn, ())
    }
    if not candidate_positions:
        return [
            "unmatched",
            None,
            0,
            *([None] * (len(SCIMAGO_OUTPUT_COLUMNS) - 3)),
        ]

    candidates = [scimago_index.records[position] for position in candidate_positions]
    candidates.sort(
        key=lambda record: (
            -len(article_issns.intersection(record.normalized_issns)),
            scimago_rank_sort_value(record.values["scimago_rank"]),
            record.position,
        )
    )
    selected_record = candidates[0]
    matched_issns = ISSN_SEPARATOR.join(
        sorted(article_issns.intersection(selected_record.normalized_issns))
    )
    match_status = "matched_issn" if len(candidates) == 1 else "matched_issn_multiple"
    return [
        match_status,
        matched_issns,
        len(candidates),
        *(selected_record.values[column] for column in SCIMAGO_OUTPUT_COLUMNS[3:]),
    ]


def write_ordered_csv(
    connection: sqlite3.Connection,
    output_path: Path,
    scimago_index: ScimagoIndex | None = None,
) -> None:
    """Atomically regenerate the ordered CSV from SQLite and the input table.

    Uncached rows are retained with blank result columns so output cardinality
    always equals the number of unique input PMIDs.

    Args:
        connection: Open cache connection with ``input_pmids`` loaded.
        output_path: Destination CSV path.
        scimago_index: Optional SCImago lookup appended to every article row.
    """
    output_path.parent.mkdir(parents=True, exist_ok=True)
    rows = connection.execute(
        """
        SELECT
            input_pmids.pmid,
            pmc_cache.pmcid,
            CASE
                WHEN pmc_cache.pmid IS NULL THEN NULL
                WHEN pmc_cache.pmc_fulltext_available = 1 THEN 'true'
                ELSE 'false'
            END,
            pmc_cache.release_date,
            pmc_cache.doi,
            pmid_cache.openalex_id,
            CASE
                WHEN pmid_cache.is_oa = 1 THEN 'true'
                WHEN pmid_cache.is_oa = 0 THEN 'false'
                ELSE NULL
            END,
            pmid_cache.best_oa_location,
            pmid_cache.title,
            pmid_cache.publication_year,
            pmid_cache.publication_date,
            pmid_cache.doi,
            pmid_cache.work_type,
            pmid_cache.study_designs,
            pmid_cache.cited_by_count,
            pmid_cache.source_openalex_id,
            pmid_cache.journal_name,
            pmid_cache.source_type,
            pmid_cache.issn_l,
            pmid_cache.issns,
            pmid_cache.status,
            pmid_cache.retrieved_at
        FROM input_pmids
        LEFT JOIN pmid_cache ON pmid_cache.pmid = input_pmids.pmid
        LEFT JOIN pmc_cache ON pmc_cache.pmid = input_pmids.pmid
        ORDER BY input_pmids.position
        """
    )

    temporary_name: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            newline="",
            dir=output_path.parent,
            prefix=f".{output_path.name}.",
            suffix=".tmp",
            delete=False,
        ) as output_file:
            temporary_name = output_file.name
            writer = csv.writer(output_file, lineterminator="\n")
            header = list(ARTICLE_CSV_COLUMNS)
            if scimago_index is not None:
                header.extend(SCIMAGO_OUTPUT_COLUMNS)
            writer.writerow(header)
            while batch := rows.fetchmany(10_000):
                if scimago_index is None:
                    writer.writerows(batch)
                    continue
                for row in batch:
                    writer.writerow(
                        [
                            *row,
                            *match_scimago(row[18], row[19], scimago_index),
                        ]
                    )
            output_file.flush()
            os.fsync(output_file.fileno())
        os.replace(temporary_name, output_path)
        temporary_name = None
    finally:
        if temporary_name is not None:
            try:
                Path(temporary_name).unlink()
            except FileNotFoundError:
                pass


def input_cache_counts(connection: sqlite3.Connection) -> tuple[int, int, int]:
    """Count found, not-found, and uncached records for the current input.

    Args:
        connection: Open cache connection with ``input_pmids`` loaded.

    Returns:
        Counts in found, not-found, uncached order.
    """
    row = connection.execute(
        """
        SELECT
            COALESCE(SUM(pmid_cache.status = 'found'), 0),
            COALESCE(SUM(pmid_cache.status = 'not_found'), 0),
            COALESCE(SUM(pmid_cache.pmid IS NULL), 0)
        FROM input_pmids
        LEFT JOIN pmid_cache ON pmid_cache.pmid = input_pmids.pmid
        """
    ).fetchone()
    if row is None:
        return 0, 0, 0
    return int(row[0]), int(row[1]), int(row[2])


def cache_state_counts(connection: sqlite3.Connection) -> CacheStateCounts:
    """Count fully enriched, citation-only, and uncached input PMIDs.

    Args:
        connection: Open cache connection with ``input_pmids`` loaded.

    Returns:
        Cache-completeness counts for startup reporting.
    """
    row = connection.execute(
        """
        SELECT
            COALESCE(SUM(
                pmid_cache.pmid IS NOT NULL
                AND pmid_cache.metadata_complete = 1
                AND pmid_cache.study_designs_complete = 1
                AND pmid_cache.oa_metadata_complete = 1
            ), 0),
            COALESCE(SUM(
                pmid_cache.pmid IS NOT NULL
                AND pmid_cache.metadata_complete = 1
                AND pmid_cache.study_designs_complete = 1
                AND pmid_cache.oa_metadata_complete != 1
            ), 0),
            COALESCE(SUM(
                pmid_cache.pmid IS NOT NULL
                AND pmid_cache.metadata_complete = 1
                AND pmid_cache.study_designs_complete != 1
            ), 0),
            COALESCE(SUM(
                pmid_cache.pmid IS NOT NULL
                AND pmid_cache.metadata_complete != 1
            ), 0),
            COALESCE(SUM(pmid_cache.pmid IS NULL), 0)
        FROM input_pmids
        LEFT JOIN pmid_cache ON pmid_cache.pmid = input_pmids.pmid
        """
    ).fetchone()
    if row is None:
        return CacheStateCounts(0, 0, 0, 0, 0)
    return CacheStateCounts(
        int(row[0]),
        int(row[1]),
        int(row[2]),
        int(row[3]),
        int(row[4]),
    )


def enriched_result_counts(
    connection: sqlite3.Connection,
) -> tuple[int, int, int, int, int, int]:
    """Calculate final OpenAlex and source coverage for the current input.

    Args:
        connection: Open cache connection with ``input_pmids`` loaded.

    Returns:
        Found, not-found, uncached, source, ISSN, and unique-source counts.
    """
    row = connection.execute(
        """
        SELECT
            COALESCE(SUM(pmid_cache.status = 'found'), 0),
            COALESCE(SUM(pmid_cache.status = 'not_found'), 0),
            COALESCE(SUM(pmid_cache.pmid IS NULL), 0),
            COALESCE(SUM(
                pmid_cache.status = 'found'
                AND pmid_cache.source_openalex_id IS NOT NULL
            ), 0),
            COALESCE(SUM(
                pmid_cache.status = 'found'
                AND (pmid_cache.issn_l IS NOT NULL OR pmid_cache.issns IS NOT NULL)
            ), 0),
            COUNT(DISTINCT CASE
                WHEN pmid_cache.status = 'found'
                THEN pmid_cache.source_openalex_id
            END)
        FROM input_pmids
        LEFT JOIN pmid_cache ON pmid_cache.pmid = input_pmids.pmid
        """
    ).fetchone()
    if row is None:
        return 0, 0, 0, 0, 0, 0
    return tuple(int(value) for value in row)


def print_summary(
    total_unique_pmids: int,
    connection: sqlite3.Connection,
    statistics: RequestStatistics,
    cached_records_reused: int,
    output_path: Path,
    database_path: Path,
) -> None:
    """Print final cache and request totals for the current input.

    Args:
        total_unique_pmids: Number of ordered unique input PMIDs.
        connection: Open cache connection with ``input_pmids`` loaded.
        statistics: Per-run HTTP counters.
        cached_records_reused: Records skipped rather than refreshed.
        output_path: Generated CSV path.
        database_path: Canonical SQLite cache path.
    """
    (
        found_count,
        not_found_count,
        uncached_count,
        source_count,
        issn_count,
        unique_source_count,
    ) = enriched_result_counts(connection)
    print("\nRun summary")
    print(f"Total PMIDs: {total_unique_pmids:,}")
    print(f"Works found: {found_count:,}")
    print(f"PMIDs not found: {not_found_count:,}")
    print(f"Works with journal/source: {source_count:,}")
    print(f"Works with ISSN: {issn_count:,}")
    print(f"Unique OpenAlex sources: {unique_source_count:,}")
    if uncached_count:
        print(f"Uncached after this run: {uncached_count:,}")
    print(f"API calls made this run: {statistics.http_requests:,}")
    print(
        f"Successful lookup calls this run: {statistics.successful_lookup_requests:,}"
    )
    print(f"Cached records reused: {cached_records_reused:,}")
    print(f"Output CSV path: {output_path.resolve()}")
    print(f"SQLite cache path: {database_path.resolve()}")


def main(argv: Sequence[str] | None = None) -> int:
    """Run the cache-first OpenAlex citation retrieval workflow.

    Args:
        argv: Optional command-line argument sequence.

    Returns:
        Process exit status.
    """
    args = parse_arguments(argv)
    load_local_environment()
    try:
        total_input_pmids, unique_pmids = read_pmids(args.pmid_file)
        scimago_index = read_scimago_index(args.scimago) if args.scimago else None
        connection = connect_database(args.db)
    except (InputError, OSError, sqlite3.Error) as error:
        print(f"Error: {error}", file=sys.stderr)
        return 2

    statistics = RequestStatistics()
    failed_batches = 0
    interrupted = False
    fatal_error: FatalApiError | None = None

    try:
        load_input_table(connection, unique_pmids)
        pending_pmids = select_pmids_to_query(
            connection,
            args.refresh_days,
            args.refresh_all,
        )
        cached_records_reused = len(unique_pmids) - len(pending_pmids)
        planned_calls = math.ceil(len(pending_pmids) / args.batch_size)
        cache_counts = cache_state_counts(connection)

        print(f"Input PMIDs: {total_input_pmids:,}")
        print(f"Unique PMIDs: {len(unique_pmids):,}")
        print(f"Fully cached: {cache_counts.fully_cached:,}")
        print(f"OA metadata pending: {cache_counts.oa_metadata_pending:,}")
        print(f"Study-design pending: {cache_counts.study_design_pending:,}")
        print(f"Citation-only cached: {cache_counts.citation_only:,}")
        print(f"Completely uncached: {cache_counts.completely_uncached:,}")
        print(f"PMIDs requiring lookup: {len(pending_pmids):,}")
        print(f"Planned OpenAlex calls: {planned_calls:,}")
        if scimago_index is not None:
            print(f"SCImago rows indexed: {len(scimago_index.records):,}")

        if pending_pmids:
            api_key = os.environ.get("OPENALEX_API_KEY")
            if not api_key or not api_key.strip():
                print(
                    "Error: OPENALEX_API_KEY is not set. No API requests were made.",
                    file=sys.stderr,
                )
                write_ordered_csv(connection, args.output, scimago_index)
                print_summary(
                    len(unique_pmids),
                    connection,
                    statistics,
                    cached_records_reused,
                    args.output,
                    args.db,
                )
                return 2
            try:
                _, failed_batches = retrieve_pending_pmids(
                    connection,
                    pending_pmids,
                    args.batch_size,
                    args.max_retries,
                    api_key,
                    statistics,
                )
            except FatalApiError as error:
                fatal_error = error

        write_ordered_csv(connection, args.output, scimago_index)
        print_summary(
            len(unique_pmids),
            connection,
            statistics,
            cached_records_reused,
            args.output,
            args.db,
        )
    except KeyboardInterrupt:
        interrupted = True
        print(
            "\nInterrupted. Completed batches are saved in SQLite.",
            file=sys.stderr,
        )
        try:
            write_ordered_csv(connection, args.output, scimago_index)
            print(f"Regenerated partial CSV: {args.output.resolve()}", file=sys.stderr)
        except (OSError, sqlite3.Error) as error:
            print(f"Could not regenerate partial CSV: {error}", file=sys.stderr)
    except (OSError, sqlite3.Error) as error:
        print(f"Error: {error}", file=sys.stderr)
        return 2
    finally:
        connection.close()

    if interrupted:
        return 130
    if fatal_error is not None:
        print(f"Fatal API error: {fatal_error}", file=sys.stderr)
        return 2
    if failed_batches:
        print(
            f"Completed with {failed_batches} failed batch(es); their PMIDs remain "
            "uncached for the next run.",
            file=sys.stderr,
        )
        return 1
    return 0


openalex_main = main

# UNIFIED COMMAND-LINE WORKFLOW

CLI_DEFAULT_DATABASE_PATH = Path("annotate_pmids_citations_impact.sqlite")
CLI_DEFAULT_OUTPUT_SUFFIX = "_citations_impact.csv"
CLI_DEFAULT_PMC_BATCH_SIZE = 200
CLI_DEFAULT_OPENALEX_BATCH_SIZE = 100
CLI_DEFAULT_MAX_RETRIES = 6
CLI_DEFAULT_PMC_DELAY_SECONDS = 0.4
CLI_DEFAULT_NCBI_TOOL_NAME = "openalex_citations_pipeline"


def warn_missing_environment(ncbi_email: str | None) -> None:
    """Load local configuration and warn about missing API configuration.

    Missing values are warnings because a fully cached run needs no API calls.

    Args:
        ncbi_email: Optional NCBI email supplied explicitly on the command line.
    """
    loaded_paths = load_local_environment()
    if not loaded_paths:
        checked_paths = ", ".join(str(path) for path in dotenv_search_paths())
        print(
            f"Warning: configuration file not found; checked: {checked_paths}",
            file=sys.stderr,
        )

    if not os.environ.get("OPENALEX_API_KEY", "").strip():
        print(
            "Warning: OPENALEX_API_KEY is missing; uncached OpenAlex lookups "
            "will fail.",
            file=sys.stderr,
        )
    effective_ncbi_email = ncbi_email or os.environ.get("NCBI_EMAIL")
    if not effective_ncbi_email or not effective_ncbi_email.strip():
        print(
            "Warning: NCBI_EMAIL is missing; PMC requests will run without a "
            "contact email.",
            file=sys.stderr,
        )


def cli_nonnegative_integer(value: str) -> int:
    """Parse a nonnegative integer argument.

    Args:
        value: Argument text to parse.

    Returns:
        Parsed nonnegative integer.

    Raises:
        argparse.ArgumentTypeError: If the value is not a nonnegative integer.
    """
    try:
        parsed_value = int(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError("must be an integer") from error
    if parsed_value < 0:
        raise argparse.ArgumentTypeError("must be zero or greater")
    return parsed_value


def cli_nonnegative_float(value: str) -> float:
    """Parse a nonnegative floating-point argument.

    Args:
        value: Argument text to parse.

    Returns:
        Parsed nonnegative number.

    Raises:
        argparse.ArgumentTypeError: If the value is not a nonnegative number.
    """
    try:
        parsed_value = float(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError("must be a number") from error
    if parsed_value < 0:
        raise argparse.ArgumentTypeError("must be zero or greater")
    return parsed_value


def cli_bounded_batch_size(value: str, maximum: int) -> int:
    """Parse a batch size bounded by an API-specific maximum.

    Args:
        value: Argument text to parse.
        maximum: Largest accepted batch size.

    Returns:
        Parsed batch size.

    Raises:
        argparse.ArgumentTypeError: If the size is outside the valid range.
    """
    parsed_value = cli_nonnegative_integer(value)
    if not 1 <= parsed_value <= maximum:
        raise argparse.ArgumentTypeError(f"must be between 1 and {maximum}")
    return parsed_value


def run_pipeline(args: argparse.Namespace) -> int:
    """Populate PMC cache data, then write OpenAlex and SCImago annotations.

    Args:
        args: Validated command-line arguments.

    Returns:
        Process exit status from the first failed stage, or zero on success.
    """
    warn_missing_environment(args.ncbi_email)

    pmc_arguments = [
        str(args.pmid_file),
        "--db",
        str(args.db),
        "--batch-size",
        str(args.pmc_batch_size),
        "--max-retries",
        str(args.max_retries),
        "--delay",
        str(args.pmc_delay),
        "--tool",
        args.ncbi_tool,
    ]
    if args.ncbi_email:
        pmc_arguments.extend(("--email", args.ncbi_email))
    if args.refresh_pmc:
        pmc_arguments.append("--refresh-all")

    print("Stage 1 of 2: PMC ID conversion")
    pmc_status = pmc_main(pmc_arguments)
    if pmc_status != 0:
        print("PMC conversion did not complete; OpenAlex stage was not started.")
        return pmc_status

    openalex_arguments = [
        str(args.pmid_file),
        "--db",
        str(args.db),
        "--output",
        str(args.output),
        "--scimago",
        str(args.scimago),
        "--batch-size",
        str(args.openalex_batch_size),
        "--max-retries",
        str(args.max_retries),
    ]
    if args.refresh_openalex:
        openalex_arguments.append("--refresh-all")
    elif args.refresh_days is not None:
        openalex_arguments.extend(("--refresh-days", str(args.refresh_days)))

    print("\nStage 2 of 2: OpenAlex and SCImago annotation")
    return openalex_main(openalex_arguments)


def main(argv: Sequence[str] | None = None) -> int:
    """Parse arguments and run the complete PMID annotation workflow.

    Args:
        argv: Optional argument sequence. Uses command-line arguments if omitted.

    Returns:
        Process exit status.
    """
    parser = argparse.ArgumentParser(
        description=(
            "Annotate a PMID list with PMC availability, OpenAlex citation and "
            "publication metadata, and SCImago journal-impact metadata."
        )
    )
    parser.add_argument(
        "pmid_file",
        type=Path,
        help="one-PMID-per-line input file",
    )
    parser.add_argument(
        "-o",
        "--output",
        type=Path,
        help=(f"output CSV (default: INPUT_STEM{CLI_DEFAULT_OUTPUT_SUFFIX})"),
    )
    parser.add_argument(
        "-d",
        "--db",
        type=Path,
        default=CLI_DEFAULT_DATABASE_PATH,
        help=f"shared durable SQLite cache (default: {CLI_DEFAULT_DATABASE_PATH})",
    )
    parser.add_argument(
        "-s",
        "--scimago",
        type=Path,
        required=True,
        help="semicolon-delimited SCImago CSV",
    )
    parser.add_argument(
        "--pmc-batch-size",
        type=lambda value: cli_bounded_batch_size(value, 200),
        default=CLI_DEFAULT_PMC_BATCH_SIZE,
        metavar="N",
        help=f"PMIDs per PMC request, 1-200 (default: {CLI_DEFAULT_PMC_BATCH_SIZE})",
    )
    parser.add_argument(
        "--openalex-batch-size",
        type=lambda value: cli_bounded_batch_size(value, 100),
        default=CLI_DEFAULT_OPENALEX_BATCH_SIZE,
        metavar="N",
        help=(
            "PMIDs per OpenAlex request, 1-100 "
            f"(default: {CLI_DEFAULT_OPENALEX_BATCH_SIZE})"
        ),
    )
    parser.add_argument(
        "--max-retries",
        type=cli_nonnegative_integer,
        default=CLI_DEFAULT_MAX_RETRIES,
        metavar="N",
        help=f"retries after an API request fails (default: {CLI_DEFAULT_MAX_RETRIES})",
    )
    parser.add_argument(
        "--pmc-delay",
        type=cli_nonnegative_float,
        default=CLI_DEFAULT_PMC_DELAY_SECONDS,
        metavar="SECONDS",
        help=(
            "delay after successful PMC requests "
            f"(default: {CLI_DEFAULT_PMC_DELAY_SECONDS})"
        ),
    )
    parser.add_argument(
        "--ncbi-email",
        help="NCBI contact email; defaults to NCBI_EMAIL from .env",
    )
    parser.add_argument(
        "--ncbi-tool",
        default=CLI_DEFAULT_NCBI_TOOL_NAME,
        help=f"NCBI tool identifier (default: {CLI_DEFAULT_NCBI_TOOL_NAME})",
    )
    parser.add_argument(
        "--refresh-pmc",
        action="store_true",
        help="re-query PMC even when a PMID is already cached",
    )
    openalex_refresh_group = parser.add_mutually_exclusive_group()
    openalex_refresh_group.add_argument(
        "--refresh-openalex",
        action="store_true",
        help="re-query OpenAlex for every input PMID",
    )
    openalex_refresh_group.add_argument(
        "--refresh-days",
        type=cli_nonnegative_integer,
        metavar="N",
        help="re-query found OpenAlex records older than N days",
    )
    args = parser.parse_args(argv)
    if args.output is None:
        args.output = args.pmid_file.with_name(
            f"{args.pmid_file.stem}{CLI_DEFAULT_OUTPUT_SUFFIX}"
        )
    return run_pipeline(args)


if __name__ == "__main__":
    raise SystemExit(main())
