# PMID Citation and Impact Annotation

`annotate_pmids_citations_impact.py` annotates a PMID list with PMC full-text
availability, OpenAlex publication and citation metadata, and SCImago journal
impact metadata.

The script is self-contained: it includes both API workflows and does not import
`pmc_idconv.py` or `openalex_citations.py`. PEP 723 metadata lets `uv` install
its Python dependencies automatically.

## Quick start

Create a `.env` file in the directory where you run the script (normally the
project root) or beside the script:

```dotenv
OPENALEX_API_KEY=your_openalex_api_key
NCBI_EMAIL=your_email@example.org
```

Run the annotation:

```bash
./annotate_pmids_citations_impact.py pmids.txt \
  --scimago scimagojr.csv
```

The default output is `pmids_citations_impact.csv`. Its name is derived from the
input filename.

To specify every important path explicitly:

```bash
./annotate_pmids_citations_impact.py pmids.txt \
  --scimago scimagojr.csv \
  --output annotated_pmids.csv \
  --db annotation_cache.sqlite
```

## Requirements

- `uv`
- Python 3.14 or newer, installed or downloadable by `uv`
- An OpenAlex API key for uncached OpenAlex requests
- An internet connection for uncached PMC and OpenAlex requests

The PEP 723 block declares these packages:

- `python-dotenv>=1.2.1`
- `requests>=2.32.5`

No separate virtual-environment setup or package installation is required when
the script is run directly.

## Inputs

### PMID file

The positional input must be a UTF-8 text file containing one numeric PMID per
line. Blank lines are ignored. Duplicate PMIDs are removed while preserving
their first-seen order. A nonnumeric value stops the run with an input error.

Example:

```text
25027
32780
76910
```

### SCImago file

`--scimago` is required. It must point to a semicolon-delimited SCImago CSV with
these columns:

- `Rank`
- `Sourceid`
- `Title`
- `Type`
- `Issn`
- `Publisher`
- `SJR`
- `SJR Best Quartile`
- `H index`
- `Country`
- `Region`
- `Coverage`
- `Categories`
- `Areas`

The script normalizes ISSNs and matches each OpenAlex journal against the
SCImago records. When multiple records match, the lowest numeric SCImago rank is
selected deterministically.

### Environment configuration

The script checks for `.env` both in the current working directory and in the
directory containing the script. If both exist, values from the current working
directory take precedence.

- `OPENALEX_API_KEY` is required when any PMID needs an OpenAlex lookup.
- `NCBI_EMAIL` supplies the contact email sent to the PMC ID Converter API. It
  is recommended but not required. `--ncbi-email` overrides it.

Missing configuration is reported before either processing stage. A fully
cached run can still succeed without an OpenAlex API key.

## Processing workflow

The script performs two stages:

1. Query the NCBI PMC ID Converter for PMC IDs, current full-text availability,
   release dates, and PMC-reported DOIs.
2. Query OpenAlex for publication, open-access, study-design, source, ISSN, and
   citation metadata; match the resulting journal ISSNs to SCImago; then write
   the ordered CSV.

Requests are batched up to 200 PMIDs for PMC and 100 PMIDs for OpenAlex. Failed
transient requests use bounded retries and server-directed or exponential
backoff.

## SQLite cache

Both stages use the same durable SQLite database. Its default name is
`annotate_pmids_citations_impact.sqlite`; use `--db` to choose another path.

The database provides:

- resumability after interruption or an API failure;
- reuse of completed API lookups;
- fewer API requests on repeated runs;
- a stable metadata snapshot unless refresh options are requested;
- storage shared between the PMC and OpenAlex stages.

The output CSV can be regenerated from cached records without repeating API
calls. Because citation counts change over time, cached counts do not update
automatically.

## Refreshing metadata

Refresh all cached OpenAlex records, including citation counts:

```bash
./annotate_pmids_citations_impact.py pmids.txt \
  --scimago scimagojr.csv \
  --refresh-openalex
```

Refresh only OpenAlex records older than a chosen number of days:

```bash
./annotate_pmids_citations_impact.py pmids.txt \
  --scimago scimagojr.csv \
  --refresh-days 30
```

Refresh PMC status as well:

```bash
./annotate_pmids_citations_impact.py pmids.txt \
  --scimago scimagojr.csv \
  --refresh-pmc \
  --refresh-openalex
```

`--refresh-openalex` and `--refresh-days` are mutually exclusive.

## Output

The CSV retains input PMID order and contains one row per unique PMID. Major
field groups include:

- PMID, PMCID, PMC full-text availability, release date, and PMC DOI;
- OpenAlex ID, open-access status and location, title, publication date, DOI,
  work type, study designs, and citation count;
- journal name, source type, OpenAlex source ID, ISSN-L, and ISSNs;
- lookup status and retrieval timestamp;
- SCImago match status, rank, source ID, title, publisher, SJR, best quartile,
  H-index, geography, coverage, categories, and subject areas.

A PMID that OpenAlex cannot resolve remains in the output with `not_found`
status and blank publication fields.

## Command-line options

```text
usage: annotate_pmids_citations_impact.py [-h] [-o OUTPUT] [-d DB] -s SCIMAGO
                                          [--pmc-batch-size N]
                                          [--openalex-batch-size N]
                                          [--max-retries N]
                                          [--pmc-delay SECONDS]
                                          [--ncbi-email NCBI_EMAIL]
                                          [--ncbi-tool NCBI_TOOL]
                                          [--refresh-pmc]
                                          [--refresh-openalex |
                                          --refresh-days N]
                                          pmid_file
```

Run `./annotate_pmids_citations_impact.py --help` for descriptions and current
defaults.

## Reproducing the existing annotation

```bash
./annotate_pmids_citations_impact.py net_pmids_noexp_20260930.txt \
  --scimago scimagojr_2025.csv \
  --output openalex_citations_scimago_noexp_20260930_oa.csv \
  --db openalex_cache.sqlite
```

Without refresh flags, this command reuses cached records. Add
`--refresh-openalex` if current citation and OpenAlex metadata are required.
Live API results can differ from an older CSV because OpenAlex metadata, open
access status, publication dates, and citation counts can change.

## Verification

The standalone script was tested on a seeded random sample of 100 PMIDs using a
fresh SQLite database. The run made one PMC request and one OpenAlex request,
produced 100 rows with the expected 39-column schema, and matched the earlier
two-script workflow apart from retrieval timestamps.
