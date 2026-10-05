import unittest
import service


class RegressionTests(unittest.TestCase):

    def test_valid_permission(self):
        self.assertTrue(
            service.authorize(
                {"subject": "u1", "expires_at": 101, "permissions": ["read"]},
                "read",
                100,
            )
        )

    def test_expired_token(self):
        self.assertFalse(
            service.authorize(
                {"subject": "u1", "expires_at": 99, "permissions": ["read"]},
                "read",
                100,
            )
        )

    def test_exact_expiry(self):
        self.assertFalse(
            service.authorize(
                {"subject": "u1", "expires_at": 100, "permissions": ["read"]},
                "read",
                100,
            )
        )

    def test_missing_permission(self):
        self.assertFalse(
            service.authorize(
                {"subject": "u1", "expires_at": 101, "permissions": ["read"]},
                "write",
                100,
            )
        )

    def test_anonymous(self):
        self.assertFalse(
            service.authorize({"expires_at": 101, "permissions": ["read"]}, "read", 100)
        )

    def test_missing_expiry(self):
        self.assertFalse(
            service.authorize({"subject": "u1", "permissions": ["read"]}, "read", 100)
        )
