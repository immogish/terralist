<script lang="ts">
  import Navbar from '@/components/Navbar.svelte';
  import Artifact from '@/components/Artifact.svelte';
  import { Artifacts } from '@/api/artifacts';
  import ErrorModal from '@/components/ErrorModal.svelte';

  export let params: {
    namespace: string,
    name: string,
    provider?: string,
    version?: string
  };

  let errorMessage: string = "";


  const onVersionDelete = async (namespace: string, name: string, provider: string | undefined, version: string) => {
    let result = await Artifacts.delete(namespace, name, provider, version);
    if (result.status === 'OK') {

    } else {
      errorMessage = result.message;
    }

  };
</script>

<Navbar />
{#key params}
  <Artifact
    type={params.provider ? "module" : "provider"}
    namespace={params.namespace.toLowerCase()}
    name={params.name.toLowerCase()}
    provider={params.provider?.toLowerCase()}
    version={params.version}
    onDelete={onVersionDelete}
  />
{/key}


{#if errorMessage}
  <ErrorModal bind:message={errorMessage} />
{/if}
