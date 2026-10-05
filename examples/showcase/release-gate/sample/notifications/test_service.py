import unittest
import service


class RegressionTests(unittest.TestCase):

    def test_repeated_delivery(self):
        self.assertEqual(len(service.deduplicate([{"idempotency_key": "a"}] * 3)), 1)

    def test_distinct_delivery(self):
        self.assertEqual(
            len(
                service.deduplicate(
                    [{"idempotency_key": "a"}, {"idempotency_key": "b"}]
                )
            ),
            2,
        )

    def test_first_payload_wins(self):
        self.assertEqual(
            service.deduplicate(
                [
                    {"idempotency_key": "a", "body": 1},
                    {"idempotency_key": "a", "body": 2},
                ]
            )[0]["body"],
            1,
        )

    def test_preserves_order(self):
        self.assertEqual(
            [
                e["idempotency_key"]
                for e in service.deduplicate(
                    [{"idempotency_key": "b"}, {"idempotency_key": "a"}]
                )
            ],
            ["b", "a"],
        )

    def test_does_not_mutate_input(self):
        events = [{"idempotency_key": "a", "body": 1}]
        result = service.deduplicate(events)
        result[0]["body"] = 2
        self.assertEqual(events[0]["body"], 1)

    def test_empty_key(self):
        with self.assertRaises(ValueError):
            service.deduplicate([{"idempotency_key": ""}])
