// Every piece of text in the interface lives here, so a translation is one
// more file of the same shape.
export default {
	'app.name': 'Zelie',
	'app.by': 'by Cariacore',

	'common.continue': 'Continue',
	'common.cancel': 'Cancel',
	'common.email': 'Email',
	'common.password': 'Password',
	'common.error': 'Something went wrong. Try again.',

	'setup.title': 'Set up Zelie.',
	'setup.lead': 'Create the administrator account. You will add a second step for logging in right after.',
	'setup.passwordHint': 'At least 10 characters.',
	'setup.noToken': 'This page needs the link printed on the server. Run zelie setup-link there to get a new one.',
	'setup.done': 'Setup is already complete.',

	'login.title': 'Log in.',
	'login.lead': 'Welcome back.',
	'login.submit': 'Log in',
	'login.second.title': 'One more step.',
	'login.second.passkey': 'Use your passkey',
	'login.second.code': 'Code from your authenticator app',
	'login.second.recovery': 'Use a recovery code instead',
	'login.second.recoveryLabel': 'Recovery code',
	'login.second.back': 'Back',

	'enroll.title': 'Protect your account.',
	'enroll.lead': 'Zelie runs your server, so every administrator logs in with a second step.',
	'enroll.passkey': 'Add a passkey',
	'enroll.passkeyLead': 'Face ID, Touch ID, Windows Hello or a security key.',
	'enroll.passkeyNoDomain': 'Passkeys need the panel on a domain name.',
	'enroll.totp': 'Use an authenticator app',
	'enroll.totpLead': 'Google Authenticator, 1Password, Aegis or any other.',
	'enroll.scan': 'Scan this code with the app,',
	'enroll.scanMore': 'or type the key by hand. Then enter the code it shows.',
	'enroll.code': 'Six-digit code',
	'enroll.recovery.title': 'Save your recovery codes.',
	'enroll.recovery.lead': 'Each one lets you in once if you lose your phone or key. They will not be shown again.',
	'enroll.recovery.copy': 'Copy',
	'enroll.recovery.copied': 'Copied',
	'enroll.recovery.saved': 'I have saved them',

	'nav.apps': 'Apps',
	'nav.empty': 'Nothing yet',
	'nav.new': 'New app',
	'nav.logout': 'Log out',
	'nav.theme': 'Theme',

	'home.title': 'Nothing is running yet.',
	'home.lead': 'Start a container from an image to see it here.'
} as const;
