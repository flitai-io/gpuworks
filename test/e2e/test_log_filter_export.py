"""CSV evidence rejects wrong content, encoding and escaping; not browser coverage.

E2E standard: test/e2e/browser-e2e-testing-standard.md
"""
import csv
import io
import unittest

from log_filter_export import CSV_FIELDS, validate_csv


class CsvEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.entry = {'time_ms': 1740000000123, 'level': 'warn', 'source': 'server',
            'instance': '', 'node': '', 'line': 'marker 中文,"CSV"\nsecond line'}
        text = io.StringIO(newline='')
        writer = csv.writer(text)
        writer.writerow(CSV_FIELDS)
        writer.writerow([self.entry[k] for k in ('time_ms', 'level', 'source', 'instance', 'node', 'line')])
        self.raw = b'\xef\xbb\xbf' + text.getvalue().encode('utf-8')

    def test_unicode_quotes_commas_and_newlines_roundtrip(self):
        rows = validate_csv(self.raw, [self.entry])
        self.assertEqual(rows[0], ['TimeMS', 'Level', 'Source', 'Instance', 'Node', 'Line'])
        self.assertEqual(rows[1][-1], self.entry['line'])

    def test_rejects_missing_utf8_bom(self):
        with self.assertRaisesRegex(AssertionError, 'must start with UTF-8 BOM EF BB BF'):
            validate_csv(self.raw[3:], [self.entry])

    def test_rejects_duplicate_utf8_bom(self):
        with self.assertRaisesRegex(AssertionError, 'duplicate leading UTF-8 BOM'):
            validate_csv(b'\xef\xbb\xbf' + self.raw, [self.entry])

    def test_rejects_wrong_encoding_html_and_missing_or_extra_rows(self):
        for raw in (b'\xef\xbb\xbf\xff', b'\xef\xbb\xbf<html>login</html>',
                    b'\xef\xbb\xbf' + b','.join(x.encode() for x in CSV_FIELDS), self.raw + self.raw[3:]):
            with self.subTest(raw=raw), self.assertRaises((AssertionError, UnicodeDecodeError)):
                validate_csv(raw, [self.entry])

    def test_rejects_wrong_identity_field_and_broken_escaping(self):
        for raw in (self.raw.replace(b'1740000000123', b'1740000000124'), self.raw.replace(b'""CSV""', b'"CSV"')):
            with self.subTest(raw=raw), self.assertRaises((AssertionError, csv.Error)):
                validate_csv(raw, [self.entry])


if __name__ == '__main__':
    unittest.main()
