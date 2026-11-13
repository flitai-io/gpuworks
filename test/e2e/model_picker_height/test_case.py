"""Geometry checks must reject hidden text and a control that grows to fit it."""
import copy
import unittest
from types import SimpleNamespace
from unittest.mock import patch

from .case import assert_single_line, prepare


class HeightOracleTests(unittest.TestCase):
    def setUp(self):
        self.geometry = {'control': {'top': 100, 'bottom': 132, 'height': 32},
                         'selected': {'top': 101, 'bottom': 131, 'height': 30},
                         'textLines': [{'text': 'model revision', 'top': 106, 'bottom': 126}]}

    def test_single_line_fits(self):
        assert_single_line(self.geometry, 32)

    def test_control_cannot_grow_to_fit_a_wrapped_label(self):
        geometry = copy.deepcopy(self.geometry)
        geometry['control'].update(bottom=202, height=102)
        with self.assertRaisesRegex(AssertionError, 'select grew'):
            assert_single_line(geometry, 32)

    def test_original_wrapped_label_escapes_fixed_control(self):
        geometry = copy.deepcopy(self.geometry)
        geometry['selected'].update(top=65, bottom=167, height=102)
        with self.assertRaisesRegex(AssertionError, 'selected label escapes'):
            assert_single_line(geometry, 32)

    def test_clipping_the_label_does_not_hide_a_second_text_line(self):
        geometry = copy.deepcopy(self.geometry)
        geometry['textLines'].append({'text': 'offscreen revision', 'top': 136, 'bottom': 156})
        with self.assertRaisesRegex(AssertionError, 'selected text is clipped'):
            assert_single_line(geometry, 32)

    def test_partial_prepare_retains_ownership_for_cleanup(self):
        report = {'evidence': []}
        # Two models are registered - the one the case is about and a second candidate so a keyboard
        # walk has somewhere to move to - then the read-back is made to disagree. Both ids are owned
        # from that point on, which is what the cleanup relies on.
        with patch('model_picker_height.case.http',
                   side_effect=[(201, {'id': 17}), (201, {'id': 18}), (500, {})]):
            with self.assertRaisesRegex(AssertionError, 'differs from the supplied fixture'):
                prepare(SimpleNamespace(base='http://unused'), 'token', 'owned', report, {}, None, None)
        self.assertEqual(report['owned_model_repo'], 'e2e-owned/model')
        self.assertEqual(report['owned_model_ids'], [17, 18])


if __name__ == '__main__':
    unittest.main()
