import { api } from './api';
import { Cancelled } from './errors';

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
	if (!cred) throw new Cancelled();
	return api('POST', '/2fa/passkey?name=' + encodeURIComponent(name), JSON.stringify(cred.toJSON()));
}

// usePasskey has one of the account's passkeys sign a challenge, to finish
// logging in or to confirm it is you.
export async function usePasskey(to: '/login/passkey' | '/confirm/passkey'): Promise<void> {
	const opts = await api<{ publicKey: PublicKeyCredentialRequestOptionsJSON }>('POST', to + '/options');
	const cred = (await navigator.credentials.get({
		publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(opts.publicKey)
	})) as PublicKeyCredential | null;
	if (!cred) throw new Cancelled();
	await api('POST', to, JSON.stringify(cred.toJSON()));
}

// A bare IP address cannot be a WebAuthn relying party.
export function onDomain(): boolean {
	const h = location.hostname;
	return !/^[\d.]+$/.test(h) && !h.includes(':');
}
