import { api } from './api';

// Browsers since 2024 turn WebAuthn options to and from JSON themselves, so
// no helper library is needed.

export function passkeysAvailable(): boolean {
	return typeof PublicKeyCredential !== 'undefined' && 'parseCreationOptionsFromJSON' in PublicKeyCredential;
}

export async function addPasskey(name: string): Promise<{ recovery_codes?: string[] }> {
	const opts = await api<{ publicKey: PublicKeyCredentialCreationOptionsJSON }>('POST', '/2fa/passkey/options');
	const cred = (await navigator.credentials.create({
		publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(opts.publicKey)
	})) as PublicKeyCredential | null;
	if (!cred) throw new Error('cancelled');
	return api('POST', '/2fa/passkey?name=' + encodeURIComponent(name), JSON.stringify(cred.toJSON()));
}

export async function loginWithPasskey(): Promise<void> {
	const opts = await api<{ publicKey: PublicKeyCredentialRequestOptionsJSON }>('POST', '/login/passkey/options');
	const cred = (await navigator.credentials.get({
		publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(opts.publicKey)
	})) as PublicKeyCredential | null;
	if (!cred) throw new Error('cancelled');
	await api('POST', '/login/passkey', JSON.stringify(cred.toJSON()));
}

// A bare IP address cannot be a WebAuthn relying party.
export function onDomain(): boolean {
	const h = location.hostname;
	return !/^[\d.]+$/.test(h) && !h.includes(':');
}
