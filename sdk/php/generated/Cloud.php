<?php

/**
 * This class has been generated by dagger-php-sdk. DO NOT EDIT.
 */

declare(strict_types=1);

namespace Dagger;

/**
 * Dagger Cloud configuration and state
 */
class Cloud extends Client\AbstractObject implements Client\IdAble
{
    /**
     * A unique identifier for this Cloud.
     */
    public function id(): CloudId
    {
        $leafQueryBuilder = new \Dagger\Client\QueryBuilder('id');
        return new \Dagger\CloudId((string)$this->queryLeaf($leafQueryBuilder, 'id'));
    }

    /**
     * The trace URL for the current session
     */
    public function traceURL(): string
    {
        $leafQueryBuilder = new \Dagger\Client\QueryBuilder('traceURL');
        return (string)$this->queryLeaf($leafQueryBuilder, 'traceURL');
    }
}
