import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { useCancelProviderCredentialLoginMutation, useGetProviderCredentialLoginQuery } from "@/lib/store/apis/providerCredentialsApi";
import { ProviderCredentialLoginStatus } from "@/lib/types/config";
import { CheckCircle2, Copy, ExternalLink, Loader2, TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";

interface Props {
	login: ProviderCredentialLoginStatus;
	onClose: () => void;
	onTerminalStatus: () => void;
}

export default function ProviderDeviceLoginDialog({ login, onClose, onTerminalStatus }: Props) {
	const [cancelLogin] = useCancelProviderCredentialLoginMutation();
	const terminalStatusReported = useRef(false);
	const [shouldPoll, setShouldPoll] = useState(login.status === "connecting");
	const pollingInterval = Math.max(login.poll_interval_seconds ?? 5, 1) * 1000;
	const { data: polledLogin } = useGetProviderCredentialLoginQuery(
		{ provider: login.provider, keyId: login.credential_id, loginId: login.login_id },
		{
			skip: !shouldPoll,
			pollingInterval: shouldPoll ? pollingInterval : 0,
			skipPollingIfUnfocused: false,
		},
	);
	const currentLogin = polledLogin ?? login;
	const isConnecting = currentLogin.status === "connecting";
	const isConnected = currentLogin.status === "connected";
	const isBrowserLogin = currentLogin.provider === "cursor" || !currentLogin.user_code;

	useEffect(() => {
		if (isConnecting || terminalStatusReported.current) return;
		setShouldPoll(false);
		terminalStatusReported.current = true;
		onTerminalStatus();
	}, [isConnecting, onTerminalStatus]);

	const close = () => {
		if (isConnecting) {
			void cancelLogin({
				provider: currentLogin.provider,
				keyId: currentLogin.credential_id,
				loginId: currentLogin.login_id,
			});
		}
		onClose();
	};

	const copyCode = async () => {
		if (!currentLogin.user_code) return;
		try {
			await navigator.clipboard.writeText(currentLogin.user_code);
			toast.success("Device code copied");
		} catch {
			toast.error("Could not copy the device code");
		}
	};

	const copyAuthorizationURL = async () => {
		if (!currentLogin.verification_url) return;
		try {
			await navigator.clipboard.writeText(currentLogin.verification_url);
			toast.success("Authorization URL copied");
		} catch {
			toast.error("Could not copy the authorization URL");
		}
	};

	return (
		<Dialog open onOpenChange={(open) => !open && close()}>
			<DialogContent data-testid="provider-device-login-dialog" className="sm:max-w-md">
				<DialogHeader>
					<DialogTitle>{currentLogin.provider === "cursor" ? "Connect Cursor account" : "Connect provider account"}</DialogTitle>
					<DialogDescription>
						{isBrowserLogin
							? "Complete secure browser authorization with the provider. Bifrost keeps the PKCE verifier server-side."
							: "Authorize this account directly with the provider. Bifrost only displays connection metadata here."}
					</DialogDescription>
				</DialogHeader>

				{isConnecting ? (
					<div className="space-y-4" data-testid="provider-device-login-connecting">
						<div className="bg-muted flex items-center gap-2 rounded-sm border p-3 text-sm">
							<Loader2 className="h-4 w-4 animate-spin" />
							Waiting for provider authorization
						</div>
						{isBrowserLogin ? (
							<p className="text-muted-foreground text-sm" data-testid="provider-browser-login-instructions">
								Open the authorization page in your browser and approve access. This dialog will update automatically when authorization
								completes.
							</p>
						) : null}
						{currentLogin.user_code ? (
							<div className="space-y-2">
								<p className="text-muted-foreground text-sm">Enter this one-time code on the provider page:</p>
								<div className="flex items-center gap-2">
									<code
										className="bg-muted flex-1 rounded-sm border px-4 py-3 text-center text-lg font-semibold tracking-widest"
										data-testid="provider-device-code"
									>
										{currentLogin.user_code}
									</code>
									<Button
										variant="outline"
										size="icon"
										onClick={copyCode}
										aria-label="Copy device code"
										data-testid="provider-device-code-copy"
									>
										<Copy className="h-4 w-4" />
									</Button>
								</div>
							</div>
						) : null}
						{currentLogin.verification_url ? (
							<div className="flex flex-col gap-2 sm:flex-row">
								<Button asChild className="flex-1" data-testid="provider-device-login-open">
									<a href={currentLogin.verification_url} target="_blank" rel="noreferrer">
										Open provider authorization <ExternalLink className="h-4 w-4" />
									</a>
								</Button>
								<Button variant="outline" onClick={copyAuthorizationURL} data-testid="provider-login-url-copy">
									<Copy className="h-4 w-4" /> Copy URL
								</Button>
							</div>
						) : null}
					</div>
				) : (
					<div
						className="flex items-start gap-3 rounded-sm border p-4"
						data-testid={isConnected ? "provider-device-login-connected" : "provider-device-login-error"}
					>
						{isConnected ? (
							<CheckCircle2 className="mt-0.5 h-5 w-5 text-green-600" />
						) : (
							<TriangleAlert className="text-destructive mt-0.5 h-5 w-5" />
						)}
						<div className="space-y-1">
							<div className="flex items-center gap-2 font-medium">
								{isConnected ? "Account connected" : "Authorization did not complete"}
								<Badge variant={isConnected ? "success" : "destructive"}>{currentLogin.status}</Badge>
							</div>
							{currentLogin.error_code ? <p className="text-muted-foreground text-sm">Provider error: {currentLogin.error_code}</p> : null}
						</div>
					</div>
				)}

				<DialogFooter>
					<Button variant="outline" onClick={close} data-testid="provider-device-login-close">
						{isConnecting ? "Cancel" : "Close"}
					</Button>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
}