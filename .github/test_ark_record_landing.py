import unittest
from unittest.mock import patch
import ark_record_landing as landing


class LandingTests(unittest.TestCase):
    def test_only_explicit_same_repo_ulids_close(self):
        task = '01M4FHTC0JY1NB1Z79NPKWK707'
        body = f'Closes ark:rein#72 ({task})\nCloses ark:elk#111 ({task})\nCloses ark:rein#12\n'
        self.assertEqual(landing.landing_refs('rein', '', body), ([task], []))
        self.assertEqual(landing.landing_refs('rein', '', f'Implements ark:rein#72 ({task})'), ([], [task]))

    def test_reject_unmerged_wrong_sha_and_fork_before_sync(self):
        pr = dict(merged_at='2026-10-10T00:00:00Z', merge_commit_sha='abc',
                  head={'repo': {'full_name': 'elk-work/rein'}},
                  base={'repo': {'full_name': 'elk-work/rein'}, 'ref': 'main'})
        variants = [dict(pr, merged_at=None), dict(pr, merge_commit_sha='wrong'),
                    dict(pr, head={'repo': {'full_name': 'fork/rein'}}),
                    dict(pr, head={'repo': None})]
        for variant in variants:
            with patch.object(landing, 'joined', return_value=('rein', landing.REPOSITORY_ID, '.')), \
                 patch.object(landing, 'gh', side_effect=[variant, {'default_branch': 'main'}]), \
                 patch.object(landing, 'runner') as runner:
                with self.assertRaises(RuntimeError):
                    landing.record('elk-work/rein', 1, 'abc')
                runner.assert_not_called()

    def test_backfill_skips_forks(self):
        pr = dict(number=5, merged_at='2026-10-09T00:00:00Z', merge_commit_sha='abc',
                  head={'repo': {'full_name': 'fork/rein'}},
                  base={'repo': {'full_name': 'elk-work/rein'}})
        with patch.object(landing, 'joined', return_value=('rein', landing.REPOSITORY_ID, '.')), \
             patch.object(landing, 'merged_prs', return_value=[pr]), \
             patch.object(landing, 'runner'), patch.object(landing, 'apply') as apply:
            landing.record_since('elk-work/rein', 48)
            apply.assert_not_called()


if __name__ == '__main__':
    unittest.main()
