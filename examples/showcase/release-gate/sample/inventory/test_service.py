import unittest
import service


class RegressionTests(unittest.TestCase):

    def test_partial_reservation(self):
        self.assertEqual(service.reserve(10, 3), 7)

    def test_exact_reservation(self):
        self.assertEqual(service.reserve(3, 3), 0)

    def test_zero_stock(self):
        with self.assertRaises(ValueError):
            service.reserve(0, 1)

    def test_overselling(self):
        with self.assertRaises(ValueError):
            service.reserve(3, 4)

    def test_negative_request(self):
        with self.assertRaises(ValueError):
            service.reserve(3, -1)

    def test_invalid_type(self):
        with self.assertRaises(ValueError):
            service.reserve(3, True)
