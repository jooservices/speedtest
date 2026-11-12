import { createHmac } from 'node:crypto';
import { OWNER } from '../steps/core.mjs';

// RFC 6238 code (SHA-1, 6 digits, 30 s) from the base32 key shown in Settings.
function authenticatorCode(secret, at = Date.now()) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  let bits = '';
  for (const ch of secret.replace(/[\s=]/g, '').toUpperCase()) bits += alphabet.indexOf(ch).toString(2).padStart(5, '0');
  const key = Buffer.from(bits.match(/.{8}/g).map(b => parseInt(b, 2)));
  const counter = Buffer.alloc(8); counter.writeBigUInt64BE(BigInt(Math.floor(at / 30000)));
  const mac = createHmac('sha1', key).update(counter).digest();
  const offset = mac[mac.length - 1] & 0x0f;
  return String((mac.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).padStart(6, '0');
}

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
      await page.setViewportSize({ width: 390, height: 844 });
      await page.getByRole('button', { name: 'Open menu' }).click();
      await page.locator('.sidebar.open').getByRole('button', { name: 'Sign out' }).click();
      await T.heading('Sign in').waitFor();
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

    // Two-factor sign-in from Settings > Access: every refusal is explained, sign-in then asks for the code, a recovery
    // code works once, and turning it off needs the password and a code.
    async twoFactor(password = OWNER.password) {
      await T.open('#/settings', 'Settings');
      await T.tab('Access');
      await T.press('Turn on…');
      const setup = T.dialog('Turn on two-factor sign-in');
      await T.press('Continue', setup);
      await T.see('Enter your current password.', setup);
      await T.fill('Current password', 'not the password at all', setup);
      await T.press('Continue', setup);
      await T.see('current password is not right', setup);
      T.expectRefusal(401);
      await T.fill('Current password', password, setup);
      await T.press('Continue', setup);
      const key = await setup.locator('[data-testid="two-factor-key"]').innerText();
      assert.ok(/^[A-Z2-7]{32}$/.test(key), 'the setup shows the base32 key');
      assert.equal(await setup.getByRole('img', { name: /QR code/ }).isVisible(), true, 'the QR code is shown');
      await T.press('Turn on', setup);
      await T.see('Enter the 6-digit code', setup);
      await T.fill('Code from the app', '000000', setup);
      await T.press('Turn on', setup);
      await T.see('not right', setup);
      T.expectRefusal(401);
      await T.fill('Code from the app', authenticatorCode(key), setup);
      await T.press('Turn on', setup);
      await T.see('Save these recovery codes', setup);
      const codes = (await setup.locator('.recovery-codes').innerText()).trim().split(/\s+/);
      assert.equal(codes.length, 10, 'ten recovery codes are shown once');
      await T.press('I saved them', setup);
      await T.see('Recovery codes left: 10.');

      // Sign-in now asks for the code.
      await T.signOut();
      await T.signIn(OWNER.email, password);
      await T.see('Enter the code from your authenticator app to finish signing in.');
      T.expectRefusal(401);
      await page.getByRole('button', { name: 'Sign in' }).click();
      await T.see('Enter the code from your authenticator app.');
      await page.getByLabel('Authentication code').fill('000000');
      await page.getByRole('button', { name: 'Sign in' }).click();
      await T.see('That code is not right');
      T.expectRefusal(401);
      await page.getByLabel('Authentication code').fill(codes[0]);
      await page.getByRole('button', { name: 'Sign in' }).click();
      await T.heading('Dashboard').waitFor({ timeout: 15000 });

      // The same recovery code does not work twice.
      await T.signOut();
      await T.signIn(OWNER.email, password);
      await T.see('to finish signing in');
      T.expectRefusal(401);
      await page.getByLabel('Authentication code').fill(codes[0]);
      await page.getByRole('button', { name: 'Sign in' }).click();
      await T.see('That code is not right');
      T.expectRefusal(401);
      await page.getByLabel('Authentication code').fill(codes[1]);
      await page.getByRole('button', { name: 'Sign in' }).click();
      await T.heading('Dashboard').waitFor({ timeout: 15000 });

      // Turn it off again: needs the password and a code.
      await T.open('#/settings', 'Settings');
      await T.tab('Access');
      await T.see('Recovery codes left: 8.');
      await T.press('Turn off…');
      const off = T.dialog('Turn off two-factor sign-in');
      await T.press('Turn off', off);
      await T.see('Enter your password and a code.', off);
      await T.fill('Current password', password, off);
      await T.fill('Code from the app, or a recovery code', '123456', off);
      await T.press('Turn off', off);
      await T.see('not right', off);
      T.expectRefusal(401);
      await T.fill('Code from the app, or a recovery code', codes[2], off);
      await T.press('Turn off', off);
      await T.toast('Two-factor sign-in is off').waitFor();
      await T.see('Off.');
    },
  };
}
