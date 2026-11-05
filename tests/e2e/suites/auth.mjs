import { OWNER } from '../steps/core.mjs';

export function authSuite(T) {
  const { page, assert } = T;
  return {
    // Signing in: empty form, wrong password, right password; the menu stays hidden until then.
    async signInRules() {
      await page.goto(T.url('#/signin'));
      await T.heading('Sign in').waitFor();
      assert.equal(await page.locator('.sidebar').isVisible(), false, 'the menu must be hidden before sign-in');
      await T.press('Sign in');
      await T.see('Enter your email and password.');
      await T.signIn(OWNER.email, 'not the password');
      await T.see('The email or password is not right');
      T.expectRefusal(401);
      assert.equal(await page.getByLabel('Password').inputValue(), '', 'a failed attempt clears the password');
      await T.signIn();
      await T.heading('Dashboard').waitFor({ timeout: 15000 });
      await page.getByRole('link', { name: 'Nodes' }).first().waitFor();
    },

    async signOutEndsSession() {
      await T.signOut();
      await page.goto(T.url('#/nodes'));
      await T.heading('Sign in').waitFor({ timeout: 15000 });
      assert.equal((await T.api('/api/nodes')).status, 401, 'the API refuses a signed-out visitor');
    },

    // Change password from Settings > Access: every refusal is explained, the change takes effect, other sessions end.
    async changePassword(newPassword) {
      await T.open('#/settings', 'Settings');
      await T.tab('Access');
      await T.press('Change password…');
      const dialog = T.dialog('Change password');
      await T.press('Change password', dialog);
      await T.see('Enter your current password.', dialog);
      await T.fill('Current password', OWNER.password, dialog);
      await T.fill('New password', 'short', dialog, true);
      await T.press('Change password', dialog);
      await T.see('at least 12 characters', dialog);
      await T.fill('New password', newPassword, dialog, true);
      await T.fill('New password again', newPassword + 'x', dialog);
      await T.press('Change password', dialog);
      await T.see('not the same', dialog);
      await T.fill('New password again', newPassword, dialog);
      await T.fill('Current password', 'wrong password entirely', dialog);
      await T.press('Change password', dialog);
      await T.see('current password is not right', dialog);
      T.expectRefusal(401); T.expectRefusal(403);
      await T.fill('Current password', OWNER.password, dialog);
      await T.press('Change password', dialog);
      await T.toast('Password changed').waitFor();
      // The old password no longer works; the new one does.
      await T.signOut();
      await T.signIn(OWNER.email, OWNER.password);
      await T.see('The email or password is not right');
      T.expectRefusal(401);
      await T.signIn(OWNER.email, newPassword);
      await T.heading('Dashboard').waitFor({ timeout: 15000 });
    },
  };
}
