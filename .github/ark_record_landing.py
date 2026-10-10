#!/usr/bin/env python3
"""Rein landing recorder, vendored from elk-work/elk.

Use ark-record-landing.sh in Actions to join a disposable replica of Rein's
existing Ark repository. Supports one verified merge or a backfill window.
Task references, idempotency and reopened-task handling follow Elk's recorder.
"""
import argparse
from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import re
import subprocess
from ark_coverage import evidence, gh, has_record, merged_prs, replica, repository_id

ROOT = Path(__file__).resolve().parent.parent

ULID = r'[0-9A-HJKMNP-TV-Z]{10,26}'
CLOSING = ('closes', 'fixes', 'resolves')
VERB = re.compile(r'\b(' + '|'.join(CLOSING) + r'|implements):?\s+$', re.I)
# "Closes ark:elk#1 (…), ark:elk#2 (…) and ark:elk#3 (…)": the verb carries along a list.
JOINER = re.compile(r'\s*(?:,|,?\s*and\b|&)\s*', re.I)


def landing_refs(name, title, body):
    """Which tasks in `name` a merged PR names, and what its landing does to each.

    Returns (closes, records): ULID prefixes in order of first appearance, none in both.

    closes   the title or body says `Closes|Fixes|Resolves ark:<name>#N (<ULID>)`.
             The landing is commented on the task and the task is closed.
    records  the title cites the task, or the body says `Implements ark:…`. The
             landing is commented and the task stays open: it can take several PRs.

    Two things are deliberately not a reference (ark:elk#60, 01M3QPCRBG):

    * `ark:<name>#N` with no ULID. The number is a display alias the server rewrites,
      so resolving one has put "Landed" comments on scout tasks the PR never
      touched. Only the ULID names a task.
    * A plain mention in the body. "filed as ark:watch#11 (01M3QN7N1V…)" names a bug
      the PR found, and the first-match rule this replaces recorded elk-work/watch#17
      as having landed it.

    A reference to another repository's task is ignored: this helper writes to the
    Ark repository the PR merged into.
    """
    ref = re.compile(r'(?<![\w:/-])ark:' + re.escape(name) + r'#\d+\s*\((' + ULID + r')\)')
    closes, records = [], []
    for text, cited in ((title, True), (body, False)):
        verb, end = None, None
        for match in ref.finditer(text):
            introduced = VERB.search(text, 0, match.start())
            if introduced:
                verb = introduced[1].lower()
            elif not (verb and end is not None and JOINER.fullmatch(text, end, match.start())):
                verb = None
            end = match.end()
            if verb in CLOSING:
                closes.append(match[1])
            elif verb or cited:
                records.append(match[1])
    closes = list(dict.fromkeys(closes))
    return closes, [prefix for prefix in dict.fromkeys(records) if prefix not in closes]


REPOSITORY_ID = '01M17TTM53XJKBZW2M9E04HHYP'


def joined(repo, directory=None):
    if repo != 'elk-work/rein':
        raise RuntimeError('This recorder only writes Rein landings')
    candidate = Path(directory) if directory else ROOT
    if not (candidate / '.ark/ark.db').exists():
        raise RuntimeError('No joined Ark replica; pass --ark-dir')
    return 'rein', REPOSITORY_ID, candidate


def same_repository(pr, repo):
    return (pr['head']['repo'] or {}).get('full_name') == repo and pr['base']['repo']['full_name'] == repo


def runner(directory, dry_run=False):
    def ark(*args, json_output=False):
        if dry_run and args[0] != 'sync':
            print('  would run: ark ' + ' '.join(a if '\n' not in a else repr(a.split('\n')[0] + ' …') for a in args))
            return {'id': '(new task)'} if json_output else ''
        result = subprocess.check_output(['ark', '-C', str(directory), *(['--json'] if json_output else []), *args], text=True)
        return json.loads(result) if json_output else result
    return ark


def moment(stamp):
    """Ark timestamps carry a variable number of fractional digits, so compare them parsed."""
    whole, _, fraction = stamp.rstrip('Z').partition('.')
    return datetime.fromisoformat(whole).replace(microsecond=int((fraction + '000000')[:6]))


def apply(ark, directory, manifest_id, repo, pr, sha):
    """Write one verified landing into a synced replica; return what was done."""
    name = repo.removeprefix('elk-work/')
    closes, records = landing_refs(name, pr['title'], pr.get('body') or '')
    with replica(directory) as db:
        repo_id = repository_id(db)
        if repo_id != manifest_id:
            raise RuntimeError('Ark repository does not match manifest')
        recorded = has_record(evidence(db, repo_id), repo, pr)
        # Resolve each ULID prefix once; every write below is addressed by the full ULID.
        tasks = {}
        for prefixes, close in ((closes, True), (records, False)):
            for prefix in prefixes:
                rows = db.execute('SELECT id, status, updated_at FROM tasks WHERE repository_id=? AND id LIKE ? AND deleted_at IS NULL', (repo_id, prefix+'%')).fetchall()
                if len(rows) == 1:
                    tasks.setdefault(rows[0][0], (close, rows[0][1], rows[0][2]))
        # A task this PR closes gets the comment unless its own comments already cite
        # the PR, whatever else does: a run that mentions the PR is not a reason for
        # a task to close without saying why. A task it only records on keeps the
        # older rule, that a PR some record already cites is not recorded twice.
        cited = {}
        for task_id in tasks:
            stamps = [row[1] for row in db.execute("SELECT body, created_at FROM comments WHERE repository_id=? AND parent_type='task' AND parent_id=? AND deleted_at IS NULL", (repo_id, task_id))
                      if has_record([row[0]], repo, pr)]
            if stamps:
                cited[task_id] = max(stamps, key=lambda s: moment(s) if s else datetime.min)
    body = f"Landed {repo}#{pr['number']}: {pr['title']}\n{pr['html_url']}\nMerge SHA: {sha}\nMerged at: {pr['merged_at']}. Recorded on the merging session's behalf."
    done = []
    for task_id, (close, status, updated) in tasks.items():
        if task_id not in cited and (close or not recorded):
            ark('task', 'comment', task_id, '-b', body)
            done.append(f'recorded on task {task_id}')
        if close and status != 'closed':
            # Already cited, open, and changed since the citing comment: someone
            # reopened it after this landing closed it. A re-run, or the daily
            # backstop passing over the same PR, must not close it again.
            if task_id in cited and cited[task_id] and updated and moment(updated) > moment(cited[task_id]):
                done.append(f'left task {task_id} open: it changed after this landing was recorded on it')
                continue
            ark('task', 'close', task_id)
            done.append(f'closed task {task_id}')
    # A bot's PR with no task reference gets no Landed task: coverage never asks for one.
    if not tasks and not recorded and (pr.get('user') or {}).get('type') != 'Bot':
        task = ark('task', 'create', '-t', 'Landed: ' + pr['title'], '-b', body, json_output=True)
        ark('task', 'close', task['id'])
        done.append(f"recorded on task {task['id']}")
    return done


def record(repo, number, sha, directory=None, dry_run=False):
    name, manifest_id, directory = joined(repo, directory)
    repo = 'elk-work/' + name
    pr = gh('api', f'repos/{repo}/pulls/{number}')
    default = gh('api', f'repos/{repo}')['default_branch']
    if not same_repository(pr, repo) or not pr['merged_at'] or pr['base']['ref'] != default or pr['merge_commit_sha'] != sha:
        raise RuntimeError('PR is not merged into the default branch at the supplied SHA')
    ark = runner(directory, dry_run)
    ark('sync')
    done = apply(ark, directory, manifest_id, repo, pr, sha)
    if not done:
        print(f'{repo}#{number}: already recorded')
        return
    if not dry_run:
        ark('sync')
    print(f'{repo}#{number}: ' + '; '.join(done))


def record_since(repo, hours, grace=1, directory=None, dry_run=False):
    """Every PR merged into the default branch between `hours` and `grace` hours ago.

    The backstop for a merge whose own run failed or never started. The grace
    period leaves a PR merged in the last hour to the run its merge started, so
    the two never race to create the same Landed record.
    """
    name, manifest_id, directory = joined(repo, directory)
    repo = 'elk-work/' + name
    now = datetime.now(timezone.utc)
    stamp = lambda d: d.strftime('%Y-%m-%dT%H:%M:%SZ')
    prs = [pr for pr in merged_prs(repo, stamp(now - timedelta(hours=hours)), stamp(now - timedelta(hours=grace)), bots=True)
           if same_repository(pr, repo)]
    ark = runner(directory, dry_run)
    ark('sync')
    wrote = False
    for pr in sorted(prs, key=lambda pr: pr['merged_at']):
        done = apply(ark, directory, manifest_id, repo, pr, pr['merge_commit_sha'])
        wrote = wrote or bool(done)
        print(f"{repo}#{pr['number']}: " + ('; '.join(done) if done else 'already recorded'))
    if wrote and not dry_run:
        ark('sync')
    print(f'{repo}: {len(prs)} merged PRs between {hours}h and {grace}h ago')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('repo')
    parser.add_argument('pr_number', type=int, nargs='?')
    parser.add_argument('merge_sha', nargs='?')
    parser.add_argument('--since-hours', type=int, help='record every PR merged in this window instead of one PR')
    parser.add_argument('--grace-hours', type=int, default=1, help='with --since-hours, skip PRs merged more recently than this')
    parser.add_argument('--ark-dir', help='a joined Rein Ark replica')
    parser.add_argument('--dry-run', action='store_true', help='sync and print what would be written; write nothing')
    args = parser.parse_args()
    if args.since_hours:
        if args.pr_number or args.merge_sha:
            parser.error('--since-hours takes no PR')
        record_since(args.repo, args.since_hours, args.grace_hours, args.ark_dir, args.dry_run)
    elif args.pr_number and args.merge_sha:
        record(args.repo, args.pr_number, args.merge_sha, args.ark_dir, args.dry_run)
    else:
        parser.error('give a PR number and its merge SHA, or --since-hours')
