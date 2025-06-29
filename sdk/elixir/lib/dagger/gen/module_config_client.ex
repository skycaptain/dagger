# This file generated by `dagger_codegen`. Please DO NOT EDIT.
defmodule Dagger.ModuleConfigClient do
  @moduledoc """
  The client generated for the module.
  """

  use Dagger.Core.Base, kind: :object, name: "ModuleConfigClient"

  alias Dagger.Core.Client
  alias Dagger.Core.QueryBuilder, as: QB

  @derive Dagger.ID

  defstruct [:query_builder, :client]

  @type t() :: %__MODULE__{}

  @doc """
  The directory the client is generated in.
  """
  @spec directory(t()) :: {:ok, String.t()} | {:error, term()}
  def directory(%__MODULE__{} = module_config_client) do
    query_builder =
      module_config_client.query_builder |> QB.select("directory")

    Client.execute(module_config_client.client, query_builder)
  end

  @doc """
  The generator to use
  """
  @spec generator(t()) :: {:ok, String.t()} | {:error, term()}
  def generator(%__MODULE__{} = module_config_client) do
    query_builder =
      module_config_client.query_builder |> QB.select("generator")

    Client.execute(module_config_client.client, query_builder)
  end

  @doc """
  A unique identifier for this ModuleConfigClient.
  """
  @spec id(t()) :: {:ok, Dagger.ModuleConfigClientID.t()} | {:error, term()}
  def id(%__MODULE__{} = module_config_client) do
    query_builder =
      module_config_client.query_builder |> QB.select("id")

    Client.execute(module_config_client.client, query_builder)
  end
end

defimpl Jason.Encoder, for: Dagger.ModuleConfigClient do
  def encode(module_config_client, opts) do
    {:ok, id} = Dagger.ModuleConfigClient.id(module_config_client)
    Jason.Encode.string(id, opts)
  end
end

defimpl Nestru.Decoder, for: Dagger.ModuleConfigClient do
  def decode_fields_hint(_struct, _context, id) do
    {:ok, Dagger.Client.load_module_config_client_from_id(Dagger.Global.dag(), id)}
  end
end
