#!/usr/bin/env python3
"""Read-only coverage and shared landing-reference matching (Elk #594)."""
import json
from pathlib import Path
import re
import sqlite3
import subprocess


class GitHubError(RuntimeError):
    pass


def gh(*args):
    try:
        return json.loads(subprocess.check_output(['gh', *args], stderr=subprocess.PIPE, text=True))
    except (subprocess.CalledProcessError, ValueError) as error:
        raise GitHubError('GitHub could not be read; check token access') from error


def replica(start=None):
    directory = Path(start or Path.cwd()).resolve()
    for parent in [directory, *directory.parents]:
        db = parent / '.ark/ark.db'
        if db.is_file():
            return sqlite3.connect(db.as_uri() + '?mode=ro', uri=True)
    raise RuntimeError('No .ark/ark.db found; join and sync this repository first')


def repository_id(db):
    # config is supplied by the caller where replicas might contain other repos.
    ids = [r[0] for r in db.execute('SELECT id FROM repositories')]
    if len(ids) != 1:
        raise RuntimeError('Expected one repository in the Ark replica')
    return ids[0]


def evidence(db, repo_id):
    records = []
    for table, columns in {
        'pull_requests': 'title, body, merge_commit_sha',
        'agent_runs': 'input_summary, result_summary, result_commit_sha, metadata_json',
        'tasks': 'title, body',
        'comments': 'body',
    }.items():
        for row in db.execute(f'SELECT {columns} FROM {table} WHERE repository_id=? AND deleted_at IS NULL', (repo_id,)):
            records.append('\n'.join(str(value) for value in row if value))
    return records


def has_record(records, repo, pr):
    """Ark numbers are not GitHub numbers. Require an explicit citation or SHA.

    Do not accept bare #N, ark:repo#N, longer PR numbers, or another org's repo.
    SHA abbreviations must be >=7 chars and bounded as a complete hex token.
    """
    number = str(pr['number'])
    short = repo.split('/')[-1]
    # elk#N is an Elk List reference in this organization, not a GitHub PR.
    names = [re.escape(repo)] + ([re.escape(short)] if short != 'elk' else [])
    ref = re.compile(r'(?<![\w:/-])(?:' + '|'.join(names) + r')\s*(?:PR\s*)?#' + number + r'(?!\d)')
    explicit = re.compile(r'(?<![\w:/-])' + re.escape(short) + r'\s+PR\s*#' + number + r'(?!\d)')
    url = re.compile(r'https://github\.com/' + re.escape(repo) + r'/pull/' + number + r'(?!\d)')
    sha = pr.get('merge_commit_sha') or ''
    for record in records:
        if ref.search(record) or explicit.search(record) or url.search(record):
            return True
        if sha and any(sha.lower().startswith(s.lower()) for s in re.findall(r'(?<![\w])[a-fA-F0-9]{7,40}(?![\w])', record)):
            return True
    return False


def merged_prs(repo, cutoff, now, bots=False):
    default = gh('api', f'repos/{repo}')['default_branch']
    # REST pagination has no search API 1,000-result ceiling. updated >= merged,
    # so we can stop when the oldest updated PR on a page precedes the window.
    result = []
    page = 1
    while True:
        rows = gh('api', f'repos/{repo}/pulls?state=closed&sort=updated&direction=desc&per_page=100&page={page}')
        if not isinstance(rows, list):
            raise GitHubError('Unexpected pull-request response')
        for pr in rows:
            if (pr['merged_at'] and cutoff <= pr['merged_at'] <= now
                    and pr['base']['ref'] == default and (bots or pr['user']['type'] != 'Bot')):
                result.append(pr)
        if len(rows) < 100 or rows[-1]['updated_at'] < cutoff:
            break
        page += 1
    return result

