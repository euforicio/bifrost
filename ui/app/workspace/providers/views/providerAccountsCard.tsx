import {
	AlertDialog,
	AlertDialogAction,
	AlertDialogCancel,
	AlertDialogContent,
	AlertDialogDescription,
	AlertDialogFooter,
	AlertDialogHeader,
	AlertDialogTitle,
} from "@/components/ui/alertDialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { getErrorMessage } from "@/lib/store";
import {
	providerCredentialsApi,
	useDisconnectProviderCredentialMutation,
	useGetProviderCredentialsQuery,
	useRefreshProviderCredentialMutation,
	useStartProviderCredentialLoginMutation,
} from "@/lib/store/apis/providerCredentialsApi";
import {
	useDeleteProviderKeyMutation,
	useGetProviderKeysQuery,
	useRefreshProviderKeyModelsMutation,
	useRefreshProviderModelsMutation,
	useUpdateProviderKeyMutation,
} from "@/lib/store/apis/providersApi";
import { useAppDispatch } from "@/lib/store/hooks";
import { ModelProvider, ModelProviderKey, ProviderCredentialLoginStatus, ProviderCredentialStatus } from "@/lib/types/config";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { ChevronDown, Link2, Loader2, Pencil, Plus, RefreshCw, Trash2, Unlink } from "lucide-react";
import { ReactNode, useCallback, useMemo, useState } from "react";
import { toast } from "sonner";
import AddNewKeySheet from "../dialogs/addNewKeySheet";
import ProviderDeviceLoginDialog from "../dialogs/providerDeviceLoginDialog";
import ProviderUsageSummary from "./providerUsageSummary";

interface Props {
	provider: ModelProvider;
	headerActions?: ReactNode;
}

interface AccountRow {
	key: ModelProviderKey;
	credential: ProviderCredentialStatus;
}

const statusBadgeVariant = {
	connected: "success",
	connecting: "warning",
	expired: "warning",
	disconnected: "secondary",
	error: "destructive",
} as const;

function formatTimestamp(value?: string) {
	if (!value) return null;
	const date = new Date(value);
	if (Number.isNaN(date.getTime())) return null;
	return date.toLocaleString();
}

export default function ProviderAccountsCard({ provider, headerActions }: Props) {
	const dispatch = useAppDispatch();
	const hasUpdateProviderAccess = useRbac(RbacResource.ModelProvider, RbacOperation.Update);
	const hasDeleteProviderAccess = useRbac(RbacResource.ModelProvider, RbacOperation.Delete);
	const { data: keys = [], isLoading: keysLoading } = useGetProviderKeysQuery(provider.name);
	const { data: credentials = [], isLoading: credentialsLoading } = useGetProviderCredentialsQuery(provider.name);
	const [startLogin, { isLoading: isStartingLogin }] = useStartProviderCredentialLoginMutation();
	const [refreshCredential] = useRefreshProviderCredentialMutation();
	const [disconnectCredential] = useDisconnectProviderCredentialMutation();
	const [updateProviderKey] = useUpdateProviderKeyMutation();
	const [deleteProviderKey] = useDeleteProviderKeyMutation();
	const [refreshProviderModels, { isLoading: isRefreshingProvider }] = useRefreshProviderModelsMutation();
	const [refreshProviderKeyModels] = useRefreshProviderKeyModelsMutation();
	const [activeLogin, setActiveLogin] = useState<ProviderCredentialLoginStatus | null>(null);
	const [busyKeyId, setBusyKeyId] = useState<string | null>(null);
	const [disconnectKeyId, setDisconnectKeyId] = useState<string | null>(null);
	const [deleteKeyId, setDeleteKeyId] = useState<string | null>(null);
	const [editKeyId, setEditKeyId] = useState<string | null>(null);
	const [showAddKey, setShowAddKey] = useState(false);
	const [togglingKeyId, setTogglingKeyId] = useState<string | null>(null);
	const [refreshingKeyId, setRefreshingKeyId] = useState<string | null>(null);

	const accounts = useMemo<AccountRow[]>(() => {
		const byCredentialID = new Map(credentials.map((credential) => [credential.credential_id, credential]));
		return keys.map((key) => ({
			key,
			credential: byCredentialID.get(key.id) ?? {
				credential_id: key.id,
				provider: provider.name,
				status: "disconnected",
				version: 0,
			},
		}));
	}, [credentials, keys, provider.name]);

	const invalidateCredentials = useCallback(() => {
		dispatch(providerCredentialsApi.util.invalidateTags([{ type: "ProviderCredentials", id: provider.name }]));
	}, [dispatch, provider.name]);

	const handleConnect = async (keyId: string) => {
		setBusyKeyId(keyId);
		try {
			const login = await startLogin({
				provider: provider.name,
				keyId,
			}).unwrap();
			setActiveLogin(login);
		} catch (error) {
			toast.error("Could not start provider authorization", {
				description: getErrorMessage(error),
			});
		} finally {
			setBusyKeyId(null);
		}
	};

	const handleRefresh = async (keyId: string) => {
		setBusyKeyId(keyId);
		try {
			await refreshCredential({ provider: provider.name, keyId }).unwrap();
			toast.success("Provider account refreshed");
		} catch (error) {
			toast.error("Provider account must be reconnected", {
				description: getErrorMessage(error),
			});
		} finally {
			setBusyKeyId(null);
		}
	};

	const handleDisconnect = async () => {
		if (!disconnectKeyId) return;
		const keyId = disconnectKeyId;
		setBusyKeyId(keyId);
		try {
			await disconnectCredential({ provider: provider.name, keyId }).unwrap();
			toast.success("Provider account disconnected");
			setDisconnectKeyId(null);
		} catch (error) {
			toast.error("Could not disconnect provider account", {
				description: getErrorMessage(error),
			});
		} finally {
			setBusyKeyId(null);
		}
	};

	const handleRefreshModels = async (key?: ModelProviderKey) => {
		if (key) setRefreshingKeyId(key.id);
		try {
			if (key) {
				await refreshProviderKeyModels({
					provider: provider.name,
					keyId: key.id,
				}).unwrap();
			} else {
				await refreshProviderModels(provider.name).unwrap();
			}
			toast.success("Model list refreshed", {
				description: key ? `Re-checked ${key.name}.` : `Re-checked ${provider.name}.`,
			});
		} catch (error) {
			toast.error("Failed to refresh model list", {
				description: getErrorMessage(error),
			});
		} finally {
			if (key) setRefreshingKeyId(null);
		}
	};

	const handleEnabledChange = async (key: ModelProviderKey, enabled: boolean) => {
		setTogglingKeyId(key.id);
		try {
			await updateProviderKey({
				provider: provider.name,
				keyId: key.id,
				key: { ...key, enabled },
			}).unwrap();
			toast.success(`${key.name} ${enabled ? "enabled" : "disabled"}`);
		} catch (error) {
			toast.error("Could not update account slot", {
				description: getErrorMessage(error),
			});
		} finally {
			setTogglingKeyId(null);
		}
	};

	const handleDelete = async () => {
		if (!deleteKeyId) return;
		setBusyKeyId(deleteKeyId);
		try {
			await deleteProviderKey({
				provider: provider.name,
				keyId: deleteKeyId,
			}).unwrap();
			toast.success(isXAI ? "Credential deleted" : "Account slot deleted");
			setDeleteKeyId(null);
		} catch (error) {
			toast.error("Could not delete credential", {
				description: getErrorMessage(error),
			});
		} finally {
			setBusyKeyId(null);
		}
	};

	const isXAI = provider.name === "xai";
	const isCursor = provider.name === "cursor";
	const addLabel = isXAI ? "Add credential" : "Add account";

	return (
		<>
			{activeLogin ? (
				<ProviderDeviceLoginDialog login={activeLogin} onClose={() => setActiveLogin(null)} onTerminalStatus={invalidateCredentials} />
			) : null}
			{showAddKey ? (
				<AddNewKeySheet show onCancel={() => setShowAddKey(false)} provider={provider} keyId={null} providerName={provider.name} />
			) : null}
			{editKeyId ? (
				<AddNewKeySheet show onCancel={() => setEditKeyId(null)} provider={provider} keyId={editKeyId} providerName={provider.name} />
			) : null}
			<AlertDialog open={disconnectKeyId !== null} onOpenChange={(open) => !open && setDisconnectKeyId(null)}>
				<AlertDialogContent data-testid="provider-account-disconnect-dialog">
					<AlertDialogHeader>
						<AlertDialogTitle>Disconnect provider account?</AlertDialogTitle>
						<AlertDialogDescription>
							This removes the provider-issued account credential from Bifrost. The configured provider key remains available to reconnect.
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel disabled={busyKeyId !== null}>Cancel</AlertDialogCancel>
						<AlertDialogAction onClick={handleDisconnect} disabled={busyKeyId !== null} data-testid="provider-account-disconnect-confirm">
							Disconnect
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>
			<AlertDialog open={deleteKeyId !== null} onOpenChange={(open) => !open && setDeleteKeyId(null)}>
				<AlertDialogContent data-testid="provider-account-delete-dialog">
					<AlertDialogHeader>
						<AlertDialogTitle>Delete {isXAI ? "credential" : "account slot"}?</AlertDialogTitle>
						<AlertDialogDescription>
							This removes the routing configuration and any connected account tokens. This action cannot be undone.
						</AlertDialogDescription>
					</AlertDialogHeader>
					<AlertDialogFooter>
						<AlertDialogCancel disabled={busyKeyId !== null}>Cancel</AlertDialogCancel>
						<AlertDialogAction onClick={handleDelete} disabled={busyKeyId !== null} data-testid="provider-account-delete-confirm">
							Delete
						</AlertDialogAction>
					</AlertDialogFooter>
				</AlertDialogContent>
			</AlertDialog>

			<Card data-testid="provider-accounts-card">
				<CardHeader className="gap-2 md:grid-cols-[1fr_auto]">
					<div className="space-y-1.5">
						<CardTitle>Provider accounts</CardTitle>
						<CardDescription>
							{isXAI
								? "xAI API keys remain supported. You can also connect an xAI account to use provider-managed authorization."
								: isCursor
									? "Connect each Cursor account in your browser using secure PKCE authorization."
									: "Connect a ChatGPT account to each configured key using provider device authorization."}
						</CardDescription>
					</div>
					<div className="flex min-w-0 flex-wrap items-center gap-2">
						{headerActions}
						{hasUpdateProviderAccess ? (
							<>
								<Tooltip>
									<TooltipTrigger asChild>
										<Button
											variant="outline"
											onClick={() => handleRefreshModels()}
											disabled={isRefreshingProvider || refreshingKeyId !== null}
											aria-label="Refresh model list"
											data-testid="provider-refresh-models"
										>
											<RefreshCw className={`h-4 w-4 ${isRefreshingProvider ? "animate-spin" : ""}`} /> Refresh models
										</Button>
									</TooltipTrigger>
									<TooltipContent>Re-check the models available through every enabled credential.</TooltipContent>
								</Tooltip>
								<Button onClick={() => setShowAddKey(true)} data-testid="provider-account-add-key">
									<Plus className="h-4 w-4" /> {addLabel}
								</Button>
							</>
						) : null}
					</div>
				</CardHeader>
				<CardContent>
					{keysLoading || credentialsLoading ? (
						<div className="text-muted-foreground flex items-center gap-2 py-4 text-sm" data-testid="provider-accounts-loading">
							<Loader2 className="h-4 w-4 animate-spin" /> Loading provider accounts
						</div>
					) : accounts.length === 0 ? (
						<div className="rounded-sm border border-dashed p-5 text-sm" data-testid="provider-accounts-empty">
							<p className="font-medium">No account slots configured</p>
							<p className="text-muted-foreground mt-1">
								{isXAI
									? "Add an API key or create an empty credential to connect an xAI account."
									: "Add an account slot, then connect it here."}
							</p>
						</div>
					) : (
						<div className="divide-y rounded-sm border" data-testid="provider-account-list">
							{accounts.map(({ key, credential }) => {
								const isBusy = busyKeyId === key.id || (isStartingLogin && busyKeyId === key.id);
								const canReconnect =
									credential.status === "disconnected" || credential.status === "expired" || credential.status === "error";
								const hasApiKey = isXAI && !!(key.value?.value?.trim() || key.value?.ref?.trim());
								const refreshedAt = formatTimestamp(credential.last_refresh);
								const expiresAt = formatTimestamp(credential.expires_at);
								const hasUsage = credential.status === "connected" || hasApiKey;
								return (
									<Collapsible key={key.id} className="group" defaultOpen={false}>
										<div
											className="flex flex-col gap-3 p-4 lg:flex-row lg:flex-wrap lg:items-start lg:justify-between"
											data-testid={`provider-account-row-${key.id}`}
										>
											<div className="min-w-0 flex-1 space-y-1">
												<div className="flex flex-wrap items-center gap-2">
													<span className="truncate font-medium">{key.name}</span>
													<Badge
														variant={
															hasApiKey && credential.status === "disconnected" ? "secondary" : statusBadgeVariant[credential.status]
														}
														data-testid={`provider-account-status-${key.id}`}
													>
														{hasApiKey && credential.status === "disconnected" ? "API key" : credential.status}
													</Badge>
												</div>
												<p className="text-muted-foreground truncate font-mono text-xs" data-testid={`provider-account-id-${key.id}`}>
													{credential.account_id || key.id}
												</p>
												<p className="text-muted-foreground text-xs">
													Weight {key.weight} · {(key.enabled ?? true) ? "Enabled" : "Disabled"}
												</p>
												{refreshedAt ? <p className="text-muted-foreground text-xs">Last refreshed {refreshedAt}</p> : null}
												{expiresAt ? <p className="text-muted-foreground text-xs">Expires {expiresAt}</p> : null}
											</div>
											{hasUpdateProviderAccess || hasDeleteProviderAccess || hasUsage ? (
												<div className="flex flex-wrap items-center gap-2">
													{hasUpdateProviderAccess ? (
														<>
															<Tooltip>
																<TooltipTrigger asChild>
																	<span className="inline-flex items-center gap-2">
																		<span className="text-muted-foreground text-xs">Enabled</span>
																		<Switch
																			checked={key.enabled ?? true}
																			disabled={togglingKeyId === key.id}
																			onAsyncCheckedChange={(enabled) => handleEnabledChange(key, enabled)}
																			data-testid={`provider-account-enabled-${key.id}`}
																		/>
																	</span>
																</TooltipTrigger>
																<TooltipContent>Include this credential in request routing.</TooltipContent>
															</Tooltip>
															{canReconnect ? (
																<Button
																	onClick={() => handleConnect(key.id)}
																	disabled={isBusy}
																	data-testid={`provider-account-connect-${key.id}`}
																>
																	{isBusy ? <Loader2 className="h-4 w-4 animate-spin" /> : <Link2 className="h-4 w-4" />}
																	{credential.status === "disconnected" ? "Connect account" : "Reconnect"}
																</Button>
															) : null}
															{credential.status === "connected" ? (
																<Button
																	variant="outline"
																	onClick={() => handleRefresh(key.id)}
																	disabled={isBusy}
																	data-testid={`provider-account-refresh-${key.id}`}
																>
																	<RefreshCw className="h-4 w-4" /> Refresh
																</Button>
															) : null}
															{credential.status !== "disconnected" ? (
																<Button
																	variant="outline"
																	onClick={() => setDisconnectKeyId(key.id)}
																	disabled={isBusy}
																	data-testid={`provider-account-disconnect-${key.id}`}
																>
																	<Unlink className="h-4 w-4" /> Disconnect
																</Button>
															) : null}
															<Tooltip>
																<TooltipTrigger asChild>
																	<Button
																		variant="ghost"
																		size="icon"
																		onClick={() => handleRefreshModels(key)}
																		disabled={refreshingKeyId !== null || isRefreshingProvider || !(key.enabled ?? true)}
																		aria-label={`Refresh models for ${key.name}`}
																		data-testid={`provider-account-refresh-models-${key.id}`}
																	>
																		<RefreshCw className={`h-4 w-4 ${refreshingKeyId === key.id ? "animate-spin" : ""}`} />
																	</Button>
																</TooltipTrigger>
																<TooltipContent>Refresh models for this credential.</TooltipContent>
															</Tooltip>
															<Button
																variant="ghost"
																size="icon"
																onClick={() => setEditKeyId(key.id)}
																aria-label={`Edit ${key.name}`}
																data-testid={`provider-account-edit-${key.id}`}
															>
																<Pencil className="h-4 w-4" />
															</Button>
														</>
													) : null}
													{hasDeleteProviderAccess ? (
														<Button
															variant="ghost"
															size="icon"
															className="text-destructive hover:bg-destructive/10 hover:text-destructive"
															onClick={() => setDeleteKeyId(key.id)}
															aria-label={`Delete ${key.name}`}
															data-testid={`provider-account-delete-${key.id}`}
														>
															<Trash2 className="h-4 w-4" />
														</Button>
													) : null}
													{hasUsage ? (
														<Tooltip>
															<TooltipTrigger asChild>
																<CollapsibleTrigger asChild>
																	<Button
																		variant="ghost"
																		size="icon"
																		aria-label={`Toggle usage for ${key.name}`}
																		data-testid={`provider-account-toggle-${key.id}`}
																	>
																		<ChevronDown className="h-4 w-4 transition-transform group-data-[state=open]:rotate-180" />
																	</Button>
																</CollapsibleTrigger>
															</TooltipTrigger>
															<TooltipContent>Show or hide account usage.</TooltipContent>
														</Tooltip>
													) : null}
												</div>
											) : null}
											{hasUsage ? (
												<CollapsibleContent className="basis-full" data-testid={`provider-account-details-${key.id}`}>
													<ProviderUsageSummary provider={provider.name} credentialId={credential.credential_id} />
												</CollapsibleContent>
											) : null}
										</div>
									</Collapsible>
								);
							})}
						</div>
					)}
				</CardContent>
			</Card>
		</>
	);
}