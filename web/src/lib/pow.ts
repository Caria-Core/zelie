// solvePow answers the login puzzle the panel sets after a few wrong
// passwords: a number whose SHA-256, after the challenge, starts with
// enough zero bits. It takes about a second and stays on this device.
export async function solvePow(challenge: string, bits: number): Promise<{ challenge: string; nonce: string }> {
	const enc = new TextEncoder();
	for (let n = 0; ; n++) {
		const nonce = String(n);
		const h = new Uint8Array(await crypto.subtle.digest('SHA-256', enc.encode(`${challenge}:${nonce}`)));
		if (zeroBits(h) >= bits) return { challenge, nonce };
	}
}

function zeroBits(h: Uint8Array): number {
	let n = 0;
	for (const b of h) {
		if (b === 0) {
			n += 8;
			continue;
		}
		return n + Math.clz32(b) - 24;
	}
	return n;
}
